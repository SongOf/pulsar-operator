package lvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SongOf/pulsar-operator/internal/agent/model"
)

// Normal discovery correctly protects a VG with active holders. A benchmark
// may exempt only its own single, unmounted linear LV, never arbitrary holders,
// partitions, raw mounts, swap, RAID, or an unreadable host inventory.
func (b *Backend) verifyBenchmarkTree(ctx context.Context, req model.BenchmarkRequest, lv map[string]string) error {
	failure := errors.New("temporary LV block topology or mount protection unverified")
	disk, err := b.identity(ctx, req.Device)
	if err != nil {
		return failure
	}
	for _, reason := range disk.Reasons {
		switch reason {
		case "Filesystem/signature detected: LVM2_member", "Has partitions or dependent block devices", "Device has active holders (RAID/LVM/multipath)":
		default:
			return failure
		}
	}
	output, err := b.run(ctx, []string{"lsblk", "--json", "--bytes", "--paths", "--output", "NAME,TYPE,MAJ:MIN,MOUNTPOINTS,FSTYPE,PTTYPE", req.Device.Path})
	if err != nil {
		return failure
	}
	type block struct {
		Name       string            `json:"name"`
		Type       string            `json:"type"`
		MajorMinor string            `json:"maj:min"`
		Mounts     json.RawMessage   `json:"mountpoints"`
		FSType     *string           `json:"fstype"`
		PTType     *string           `json:"pttype"`
		Children   []json.RawMessage `json:"children"`
	}
	var tree struct {
		Devices []block `json:"blockdevices"`
	}
	if json.Unmarshal(output, &tree) != nil || len(tree.Devices) != 1 {
		return failure
	}
	unmounted := func(raw json.RawMessage) bool {
		if len(raw) == 0 {
			return false
		}
		var values []any
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if value != nil && value != "" {
				return false
			}
		}
		return true
	}
	empty := func(s *string) bool { return s == nil || *s == "" }
	root := tree.Devices[0]
	if root.Type != "disk" || len(root.Children) != 1 || !unmounted(root.Mounts) || !empty(root.PTType) || root.FSType == nil || *root.FSType != "LVM2_member" {
		return failure
	}
	var child block
	if json.Unmarshal(root.Children[0], &child) != nil || child.Type != "lvm" || len(child.Children) != 0 || !unmounted(child.Mounts) || !empty(child.FSType) || !empty(child.PTType) {
		return failure
	}
	if strings.HasPrefix(lv["lv_kernel_major"], "-") || strings.HasPrefix(lv["lv_kernel_minor"], "-") || child.MajorMinor != fmt.Sprintf("%s:%s", lv["lv_kernel_major"], lv["lv_kernel_minor"]) {
		return failure
	}
	return nil
}
