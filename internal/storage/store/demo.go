package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/google/uuid"
)

func (s *Store) Seed(ctx context.Context) error {
	created := false
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM objects WHERE kind='disks'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		created = true
		for n := 1; n <= 24; n++ {
			node := fmt.Sprintf("baremetal-%03d", n)
			for d := 0; d < 6; d++ {
				name, media, description := fmt.Sprintf("sd%c", 'a'+d), "SSD", "模拟 SATA SSD"
				if d < 3 {
					name, media, description = fmt.Sprintf("nvme%dn1", d), "NVMe", "模拟 NVMe"
				}
				var size int64 = 3840
				if d < 3 {
					size = 1920
				}
				if d == 0 {
					size = 480
				}
				state, reasons := "Available", []string{}
				if d == 0 {
					state, reasons = "Protected", []string{"模拟系统盘，禁止接管"}
				} else if n == 24 && d == 5 {
					state, reasons = "Offline", []string{"模拟设备失联"}
				}
				disk := model.Object{"id": fmt.Sprintf("disk-%03d-%d", n, d), "name": name, "node": node, "path": fmt.Sprintf("/dev/disk/by-id/demo-%03d-%d", n, d),
					"serial": fmt.Sprintf("DEMO-%03d-%d", n, d), "sizeBytes": size * model.GiB, "media": media, "model": description, "state": state, "reasons": reasons, "poolId": nil}
				if err := s.Put(ctx, tx, "disks", disk); err != nil {
					return err
				}
			}
			if err := s.Put(ctx, tx, "nodes", model.Object{"id": node, "name": node, "node": node, "zone": fmt.Sprintf("az-%d", 1+(n-1)/8), "state": "Ready", "disks": 6, "capacityBytes": 15840 * model.GiB, "allocatedBytes": 0}); err != nil {
				return err
			}
		}
		return s.Event(ctx, tx, "demo.seed", "已生成 24 台模拟节点 / 144 块模拟磁盘；不代表本机实际硬件", "", "info")
	})
	if err != nil || !created {
		return err
	}
	for i := 1; i <= 6; i++ {
		plan, err := s.Plan(ctx, model.Object{"name": fmt.Sprintf("nvme-pool-%02d", i), "node": fmt.Sprintf("baremetal-%03d", i), "diskIds": []string{fmt.Sprintf("disk-%03d-1", i)}})
		if err != nil {
			return err
		}
		pool, err := s.CreatePool(ctx, model.String(plan["planId"]), model.String(plan["confirmation"]))
		if err != nil {
			return err
		}
		if _, err = s.CreateVolume(ctx, model.Object{"name": fmt.Sprintf("app-data-%02d", i), "namespace": "default", "poolId": pool["id"], "sizeGiB": i * 64}); err != nil {
			return err
		}
	}
	return nil
}

func diskID(value any) (string, error) {
	var id any
	switch values := value.(type) {
	case []any:
		if len(values) == 1 {
			id = values[0]
		}
	case []string:
		if len(values) == 1 {
			id = values[0]
		}
	}
	text, ok := id.(string)
	if !ok {
		return "", model.NewProblem(400, "每个池仅接管一块物理盘；批量操作会创建多个独立池")
	}
	return model.Identifier(text)
}

func (s *Store) Plan(ctx context.Context, body model.Object) (model.Object, error) {
	name, err := model.Name(body["name"])
	if err != nil {
		return nil, err
	}
	node, err := model.NodeName(body["node"])
	if err != nil {
		return nil, err
	}
	id, err := diskID(body["diskIds"])
	if err != nil {
		return nil, err
	}
	var disk model.Object
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		disk, err = s.getRequired(ctx, tx, "disks", id)
		if err != nil {
			return err
		}
		if disk["node"] != node || disk["state"] != "Available" {
			return model.NewProblem(409, "磁盘不可接管或不属于选定节点")
		}
		return uniquePoolName(ctx, tx, name)
	})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(node + ":" + model.String(disk["serial"])))
	vg := "df_" + hex.EncodeToString(hash[:8])
	return s.SavePlan(ctx, model.Object{"name": name, "node": node, "diskIds": []string{id}, "disks": []model.Object{disk}, "vgName": vg, "confirmation": "接管 " + model.String(disk["serial"]),
		"commands": [][]string{{"pvcreate", model.String(disk["path"])}, {"vgcreate", vg, model.String(disk["path"])}},
		"warnings": []string{"模拟模式：不执行上述命令。", "真实模式会初始化所选空盘；LVM 不提供跨节点数据复制。"}})
}

func uniquePoolName(ctx context.Context, tx *sql.Tx, name string) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM objects WHERE kind='pools' AND name=?", name).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return model.NewProblem(409, "存储池名称已存在")
	}
	return nil
}

func (s *Store) CreatePool(ctx context.Context, planID, confirmation string) (model.Object, error) {
	var pool model.Object
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, consumed, err := s.ReadPlan(ctx, tx, planID, confirmation)
		if err != nil {
			return err
		}
		vg := model.String(plan["vgName"])
		if len(vg) < 4 || vg[:3] != "df_" {
			return fmt.Errorf("invalid persisted plan volume group")
		}
		pid := "pool-" + vg[3:]
		if consumed {
			pool, err = s.getRequired(ctx, tx, "pools", pid)
			return err
		}
		id, err := diskID(plan["diskIds"])
		if err != nil {
			return err
		}
		disk, err := s.getRequired(ctx, tx, "disks", id)
		if err != nil {
			return err
		}
		disks, ok := plan["disks"].([]any)
		if !ok || len(disks) != 1 {
			return fmt.Errorf("invalid persisted plan devices")
		}
		expected, ok := disks[0].(map[string]any)
		if !ok {
			return fmt.Errorf("invalid persisted plan identity")
		}
		if disk["state"] != "Available" || disk["serial"] != expected["serial"] || disk["path"] != expected["path"] || disk["node"] != expected["node"] || model.Int64(disk["sizeBytes"]) != model.Int64(expected["sizeBytes"]) {
			return model.NewProblem(409, "磁盘状态或身份已经改变，必须重新生成计划")
		}
		if err := uniquePoolName(ctx, tx, model.String(plan["name"])); err != nil {
			return err
		}
		capacity := (model.Int64(disk["sizeBytes"])/model.GiB - 1) * model.GiB
		if capacity <= 0 {
			return model.NewProblem(409, "磁盘容量不足，无法创建模拟存储池")
		}
		pool = model.Object{"id": pid, "name": plan["name"], "node": plan["node"], "vgName": vg, "capacityBytes": capacity, "allocatedBytes": int64(0), "availableBytes": capacity,
			"state": "Ready", "diskIds": plan["diskIds"], "storageClass": "diskfleet-" + pid, "createdAt": model.Now()}
		disk["state"], disk["poolId"] = "Managed", pid
		if err := s.Put(ctx, tx, "disks", disk); err != nil {
			return err
		}
		if err := s.Put(ctx, tx, "pools", pool); err != nil {
			return err
		}
		if err := s.ConsumePlan(ctx, tx, planID); err != nil {
			return err
		}
		return s.Event(ctx, tx, "pool.ready", fmt.Sprintf("模拟接管 %s → %s", model.String(disk["serial"]), model.String(pool["name"])), model.String(pool["node"]), "info")
	})
	return pool, err
}

func (s *Store) CreateVolume(ctx context.Context, body model.Object) (model.Object, error) {
	name, err := model.Name(body["name"])
	if err != nil {
		return nil, err
	}
	ns, exists := body["namespace"]
	if !exists {
		ns = "default"
	}
	namespace, err := model.Name(ns)
	if err != nil {
		return nil, err
	}
	size, err := model.SizeGiB(body["sizeGiB"])
	if err != nil {
		return nil, err
	}
	poolID, err := model.Identifier(body["poolId"])
	if err != nil {
		return nil, err
	}
	var volume model.Object
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		matches, err := queryObjects(ctx, tx, "SELECT data FROM objects WHERE kind='volumes' AND name=?", name)
		if err != nil {
			return err
		}
		for _, existing := range matches {
			if existing["namespace"] == namespace && existing["status"] != "Released" {
				if existing["poolId"] == poolID && model.Int64(existing["sizeBytes"]) == size {
					volume = existing
					return nil
				}
				return model.NewProblem(409, "同命名空间中已存在同名 PVC，参数不同")
			}
		}
		pool, err := s.getRequired(ctx, tx, "pools", poolID)
		if err != nil {
			return err
		}
		if pool["state"] != "Ready" || size > model.Int64(pool["availableBytes"]) {
			return model.NewProblem(409, "存储池不可用或剩余容量不足")
		}
		key := uuid.NewString()
		mount := filepath.Join(s.root, "volumes", key)
		if err := os.MkdirAll(filepath.Dir(mount), 0700); err != nil {
			return err
		}
		if err := os.Mkdir(mount, 0700); err != nil {
			return err
		}
		volume = model.Object{"id": key, "name": name, "namespace": namespace, "poolId": pool["id"], "pool": pool["name"], "node": pool["node"], "sizeBytes": size,
			"status": "Bound", "storageClass": pool["storageClass"], "pvName": "demo-pv-" + key, "mountPath": mount, "createdAt": model.Now()}
		pool["allocatedBytes"] = model.Int64(pool["allocatedBytes"]) + size
		pool["availableBytes"] = model.Int64(pool["availableBytes"]) - size
		if err := s.Put(ctx, tx, "pools", pool); err != nil {
			return err
		}
		if err := s.Put(ctx, tx, "volumes", volume); err != nil {
			return err
		}
		return s.Event(ctx, tx, "volume.bound", fmt.Sprintf("模拟 PVC %s/%s 分配 %d GiB（目录不实施容量配额）", namespace, name, size/model.GiB), model.String(pool["node"]), "info")
	})
	return volume, err
}

func (s *Store) Expand(ctx context.Context, key string, body model.Object) (model.Object, error) {
	size, err := model.SizeGiB(body["sizeGiB"])
	if err != nil {
		return nil, err
	}
	var volume model.Object
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		volume, err = s.getRequired(ctx, tx, "volumes", key)
		if err != nil {
			return err
		}
		pool, err := s.getRequired(ctx, tx, "pools", model.String(volume["poolId"]))
		if err != nil {
			return err
		}
		delta := size - model.Int64(volume["sizeBytes"])
		if volume["status"] != "Bound" || delta < 0 {
			return model.NewProblem(409, "仅允许扩容已绑定的卷，不能缩容")
		}
		if delta > model.Int64(pool["availableBytes"]) {
			return model.NewProblem(409, "存储池剩余容量不足")
		}
		volume["sizeBytes"] = size
		pool["allocatedBytes"] = model.Int64(pool["allocatedBytes"]) + delta
		pool["availableBytes"] = model.Int64(pool["availableBytes"]) - delta
		if err := s.Put(ctx, tx, "volumes", volume); err != nil {
			return err
		}
		if err := s.Put(ctx, tx, "pools", pool); err != nil {
			return err
		}
		return s.Event(ctx, tx, "volume.expanded", fmt.Sprintf("模拟 PVC %s 扩容至 %d GiB", model.String(volume["name"]), size/model.GiB), model.String(pool["node"]), "info")
	})
	return volume, err
}

func (s *Store) Release(ctx context.Context, key, expectedUID string) (model.Object, error) {
	// Demo identities are UUIDs of retained directories, not Kubernetes PVC UIDs.
	if expectedUID != "" {
		if _, err := model.Identifier(expectedUID); err != nil {
			return nil, err
		}
	}
	var volume model.Object
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		volume, err = s.getRequired(ctx, tx, "volumes", key)
		if err != nil {
			return err
		}
		volume["status"] = "Released"
		if err := s.Put(ctx, tx, "volumes", volume); err != nil {
			return err
		}
		return s.Event(ctx, tx, "volume.retained", fmt.Sprintf("PVC %s 已释放，数据和容量继续保留", model.String(volume["name"])), model.String(volume["node"]), "info")
	})
	return volume, err
}

func (s *Store) Overview(ctx context.Context) (model.Object, error) {
	totals := model.Object{"nodes": 0, "disks": 0, "pools": 0, "volumes": 0, "capacityBytes": int64(0), "allocatedBytes": int64(0), "availableBytes": int64(0)}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT kind,COUNT(*) FROM objects GROUP BY kind")
		if err != nil {
			return err
		}
		for rows.Next() {
			var kind string
			var count int
			if err := rows.Scan(&kind, &count); err != nil {
				rows.Close()
				return err
			}
			if kind == "nodes" || kind == "disks" || kind == "pools" || kind == "volumes" {
				totals[kind] = count
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		pools, err := queryObjects(ctx, tx, "SELECT data FROM objects WHERE kind='pools'")
		if err != nil {
			return err
		}
		for _, pool := range pools {
			for _, key := range []string{"capacityBytes", "allocatedBytes", "availableBytes"} {
				totals[key] = model.Int64(totals[key]) + model.Int64(pool[key])
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	events, err := s.Events(ctx, model.ListOptions{Limit: 8})
	if err != nil {
		return nil, err
	}
	return model.Object{"mode": "demo", "totals": totals, "recentEvents": events.Items, "alerts": []model.Object{
		{"level": "info", "message": "模拟环境：容量为逻辑预留，挂载路径为普通目录；未执行 LVM 或 Kubernetes 调度。"},
		{"level": "warning", "message": "baremetal-024 有一块模拟离线磁盘，请检查节点连接。"},
	}}, nil
}
