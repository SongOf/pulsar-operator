// Command storage-benchmark measures local metadata operations using a temporary
// database. It is not a Kubernetes or block-storage benchmark.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	count := flag.Int("disks", 10000, "simulated disk metadata rows (1..1000000)")
	flag.Parse()
	if *count < 1 || *count > 1000000 {
		return fmt.Errorf("--disks must be 1..1000000")
	}
	directory, err := os.MkdirTemp("", "diskfleet-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	local, err := store.New(directory)
	if err != nil {
		return err
	}
	defer local.Close()
	ctx := context.Background()
	start := time.Now()
	err = local.Transaction(ctx, func(tx *sql.Tx) error {
		for i := 0; i < *count; i++ {
			id := fmt.Sprintf("disk-%08d", i)
			if err := local.Put(ctx, tx, "disks", model.Object{"id": id, "name": id, "node": fmt.Sprintf("node-%05d", i/12), "state": "Available", "serial": fmt.Sprintf("BENCH-%d", i), "sizeBytes": 1920 * model.GiB}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	inserted := time.Since(start).Seconds()
	samples := make([]float64, 100)
	for i := range samples {
		start = time.Now()
		page, err := local.List(ctx, "disks", model.ListOptions{Limit: 50, Offset: (i * 50) % *count})
		if err != nil {
			return err
		}
		if page.Total != *count {
			return fmt.Errorf("metadata row count mismatch")
		}
		samples[i] = float64(time.Since(start).Microseconds()) / 1000
	}
	slices.Sort(samples)
	return json.NewEncoder(os.Stdout).Encode(model.Object{"scope": "SQLite metadata only; not real disk or Kubernetes performance", "disks": *count, "insertSeconds": inserted, "pageMedianMs": samples[50], "pageP95Ms": samples[95]})
}
