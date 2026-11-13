// Command storage-plan renders reviewed Pulsar manifests without applying them.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SongOf/pulsar-operator/internal/kubeconfig"
	"github.com/SongOf/pulsar-operator/internal/storage/cluster"
	"github.com/SongOf/pulsar-operator/internal/storage/fleet"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "准备失败：", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Usage = func() {
		fmt.Fprintln(os.Stdout, "Usage: storage-plan [options] (read-only; never applies resources)")
		flag.VisitAll(func(f *flag.Flag) { fmt.Fprintf(os.Stdout, "  --%s\t%s (default %q)\n", f.Name, f.Usage, f.DefValue) })
	}
	input := flag.String("pools-json", "", "offline pool snapshot (array or {items:[...]}); does not verify live Nodes")
	zk := flag.String("zookeeper-pools", "", "comma-separated reviewed ZooKeeper pools (required)")
	journal := flag.String("journal-pools", "", "comma-separated reviewed journal pools (required)")
	ledger := flag.String("ledger-pools", "", "comma-separated reviewed ledger pools (required)")
	namespace := flag.String("namespace", "", "dedicated diskfleet-pulsar-* namespace (required)")
	name := flag.String("name", "diskfleet-pulsar", "PulsarCluster name")
	replicas := flag.Int("replicas", 3, "ZooKeeper and BookKeeper replicas")
	profile := flag.String("profile", "three-node-e2e", "three-node-e2e, two-node-lab or two-node-shared-disk-lab")
	output := flag.String("output", "", "new output file (required; existing files are never overwritten)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *output == "" || *namespace == "" || *zk == "" || *journal == "" || *ledger == "" {
		return fmt.Errorf("--output, --namespace and all three --*-pools are required")
	}
	if *profile != "three-node-e2e" && !fleet.TwoNode(*profile) {
		return fmt.Errorf("不支持的 Pulsar profile：%s", *profile)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, 2*time.Minute)
	defer deadline()
	var pools []model.Object
	var client dynamic.Interface
	if *input != "" {
		data, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		var raw json.RawMessage = data
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
			var page struct {
				Items json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(data, &page); err != nil {
				return err
			}
			raw = page.Items
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&pools); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("pool snapshot must contain exactly one JSON document")
		}
	} else {
		config, err := kubeconfig.Config()
		if err != nil {
			return err
		}
		api, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return err
		}
		if _, err := api.ServerResourcesForGroupVersion("pulsar.songof.dev/v1alpha1"); err != nil {
			return err
		}
		client, err = dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		directory, err := os.MkdirTemp("", "diskfleet-pulsar-plan-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(directory)
		local, err := store.New(directory)
		if err != nil {
			return err
		}
		defer local.Close()
		page, err := cluster.New(local, client).List(ctx, "pools", model.ListOptions{Limit: 1000000})
		if err != nil {
			return err
		}
		pools = page.Items
	}
	split := func(value string) []string {
		result := []string{}
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				result = append(result, item)
			}
		}
		return result
	}
	assignments := map[string][]string{"zookeeper": split(*zk), "journal": split(*journal), "ledger": split(*ledger)}
	manifest, err := fleet.PulsarPlan(pools, assignments, *namespace, *name, *replicas, *profile)
	if err != nil {
		return err
	}
	if fleet.TwoNode(*profile) && client != nil {
		var nodes []model.Object
		options := metav1.ListOptions{Limit: 500, LabelSelector: fleet.TwoNodeLabel + "=true"}
		for {
			page, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).List(ctx, options)
			if err != nil {
				return err
			}
			for _, node := range page.Items {
				nodes = append(nodes, node.Object)
			}
			options.Continue = page.GetContinue()
			if options.Continue == "" {
				break
			}
		}
		if err := fleet.ValidateTwoNodeTargets(pools, assignments, nodes, *profile); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("输出文件已存在或不可写；选择新路径，避免覆盖已经审核的计划: %w", err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	mode := "live-read-only"
	if *input != "" {
		mode = "offline-input"
	}
	if fleet.TwoNode(*profile) {
		fmt.Fprintln(os.Stderr, *profile+"：3 ZooKeeper + 3 Bookie 使用 Preferred 反亲和，不能保证 2+1 分布；非主机故障高可用，不适用原三节点 E2E。")
		if *profile == "two-node-shared-disk-lab" {
			fmt.Fprintln(os.Stderr, "三个角色共用每节点同一块数据盘，没有物理盘或 I/O 隔离；每个池须至少 94Gi 实际可用空间。")
		}
		if *input != "" {
			fmt.Fprintln(os.Stderr, "离线输入仅供参考：未验证实时节点标签、Ready/封锁状态或真实存储池身份，不能作为已完成部署预检的证据。")
		}
	}
	return json.NewEncoder(os.Stdout).Encode(model.Object{"output": *output, "source": fleet.SourceRepository, "revision": fleet.SourceRevision, "mode": mode, "resources": len(manifest["items"].([]any)), "namespace": *namespace, "cluster": *name, "profile": *profile, "applied": false})
}
