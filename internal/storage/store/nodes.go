package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

func demoNode(node model.Object) model.Object {
	if node["uid"] == nil {
		node["uid"] = "demo-" + model.String(node["name"])
	}
	if node["resourceVersion"] == nil {
		node["resourceVersion"] = "1"
	}
	if node["enrolled"] == nil {
		node["enrolled"] = true
	}
	ready := node["state"] == "Ready"
	enrolled := node["enrolled"] == true
	reasons := []string{}
	if !ready {
		reasons = append(reasons, "模拟节点未就绪")
	}
	if node["unschedulable"] == true {
		reasons = append(reasons, "模拟节点已封锁")
	}
	node["enableReasons"] = append([]string{}, reasons...)
	node["canEnableAgent"] = !enrolled && len(reasons) == 0
	if !enrolled {
		reasons = append(reasons, "模拟节点尚未启用 Agent")
	}
	node["ready"], node["unschedulable"], node["mode"] = ready, node["unschedulable"] == true, "demo"
	node["agentStatus"] = "NotEnrolled"
	if enrolled {
		node["agentStatus"] = "Running"
	}
	node["canCreatePool"], node["reasons"] = len(reasons) == 0, reasons
	return node
}
func demoDisk(disk, node model.Object) model.Object {
	if disk["uid"] == nil {
		disk["uid"] = "demo-" + model.String(disk["id"])
	}
	if disk["resourceVersion"] == nil {
		disk["resourceVersion"] = "1"
	}
	reasons := []string{}
	switch values := disk["reasons"].(type) {
	case []any:
		for _, v := range values {
			reasons = append(reasons, model.String(v))
		}
	case []string:
		reasons = append(reasons, values...)
	}
	if node == nil {
		reasons = append(reasons, "模拟节点不存在")
	} else {
		node = demoNode(node)
		if node["canCreatePool"] != true {
			reasons = append(reasons, "模拟节点 Agent 未就绪")
		}
		disk["nodeUID"] = node["uid"]
	}
	fresh := node != nil && node["canCreatePool"] == true
	disk["mode"], disk["fresh"] = "demo", fresh
	disk["lastObservedTime"], disk["validUntil"] = "", ""
	if fresh {
		now := time.Now().UTC()
		disk["lastObservedTime"] = now.Format(time.RFC3339Nano)
		disk["validUntil"] = now.Add(36 * time.Second).Format(time.RFC3339Nano)
	}
	if disk["state"] != "Available" {
		reasons = append(reasons, "模拟磁盘不是 Available")
	}
	if model.String(disk["poolId"]) != "" {
		reasons = append(reasons, "模拟磁盘已纳管")
	}
	disk["canCreatePool"], disk["reasons"] = len(reasons) == 0, reasons
	return disk
}
func (s *Store) Node(ctx context.Context, name string, opts model.ListOptions) (model.Object, error) {
	if _, err := model.NodeName(name); err != nil {
		return nil, err
	}
	nodes, err := s.List(ctx, "nodes", model.ListOptions{Node: name, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, model.NewProblem(404, "节点不存在")
	}
	node := nodes.Items[0]
	opts.Node = name
	disks, err := s.List(ctx, "disks", opts)
	if err != nil {
		return nil, err
	}
	return model.Object{"node": demoNode(node), "disks": disks}, nil
}
func (s *Store) EnableAgent(ctx context.Context, name string, body model.Object) (model.Object, error) {
	if _, err := model.NodeName(name); err != nil {
		return nil, err
	}
	uid, err := model.Identifier(body["uid"])
	if err != nil {
		return nil, err
	}
	rv, err := model.Identifier(body["resourceVersion"])
	if err != nil {
		return nil, err
	}
	var node model.Object
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		node, err = s.getRequired(ctx, tx, "nodes", name)
		if err != nil {
			return err
		}
		node = demoNode(node)
		if uid != node["uid"] || rv != node["resourceVersion"] {
			return model.NewProblem(409, "模拟节点身份或版本已变化，请刷新后重试")
		}
		if node["enrolled"] == true {
			return nil
		}
		if node["canEnableAgent"] != true {
			return model.NewProblem(409, "模拟节点当前不可启用")
		}
		version, err := strconv.ParseUint(rv, 10, 64)
		if err != nil {
			return err
		}
		node["enrolled"], node["resourceVersion"] = true, strconv.FormatUint(version+1, 10)
		node = demoNode(node)
		if err := s.Put(ctx, tx, "nodes", node); err != nil {
			return err
		}
		return s.Event(ctx, tx, "node.agent-enabled", "模拟启用 Agent，不部署进程或操作主机", name, "info")
	})
	return node, err
}
