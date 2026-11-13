// Package lvm reconciles one-disk volume groups using the ownership journal
// written by the original DiskFleet agent. It never formats, wipes or deletes.
package lvm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SongOf/pulsar-operator/internal/agent/host"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"golang.org/x/sys/unix"
)

var (
	vgPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,126}$`)
	partitionPattern = regexp.MustCompile(`-part[0-9]+$`)
)

// Backend is configured once at startup. WritesEnabled is the process-wide
// opt-in, independently checked here in addition to the caller's approval gate.
// Inventory and ValidateDevice can be replaced by doubles for offline tests.
type Backend struct {
	StateDir       string
	Runner         host.Runner
	Inventory      func(context.Context) ([]model.Disk, error)
	ValidateDevice func(string) error
	WritesEnabled  bool
}

// journal's field names and phases are intentionally identical to Python's
// diskfleet.storage.LVMBackend. Missing poolUID denotes the older journal format.
type journal struct {
	Identity model.Device `json:"identity"`
	VGName   string       `json:"vgName"`
	PoolUID  *string      `json:"poolUID,omitempty"`
	PVUUID   string       `json:"pvUUID"`
	Tag      string       `json:"tag"`
	Phase    string       `json:"phase"`
}

func (b *Backend) run(ctx context.Context, args []string) ([]byte, error) {
	if b.Runner != nil {
		return b.Runner(ctx, args)
	}
	return host.Run(ctx, args)
}

// EnsurePool verifies ownership before each initialization step. Successful
// commands followed by a crash are recovered using a preallocated PV UUID and
// VG tag; an owned object that has disappeared is never recreated automatically.
func (b *Backend) EnsurePool(ctx context.Context, vgName string, devices []model.Device, approved bool, ownerUID string, allowLegacyMigration bool) (model.PoolResult, error) {
	if !approved || !b.WritesEnabled {
		return model.PoolResult{}, errors.New("disk writes disabled: explicit approval and ENABLE_DISK_WRITES=true are required")
	}
	if !vgPattern.MatchString(vgName) || vgName == "all" || vgName == "default" {
		return model.PoolResult{}, errors.New("invalid volume group name")
	}
	if len(devices) != 1 {
		return model.PoolResult{}, errors.New("exactly one disk is required per pool")
	}
	if strings.TrimSpace(ownerUID) == "" {
		return model.PoolResult{}, errors.New("a DiskPool UID is required for durable ownership")
	}
	if b.StateDir == "" || b.Inventory == nil {
		return model.PoolResult{}, errors.New("state directory and disk inventory are required")
	}
	if err := ctx.Err(); err != nil {
		return model.PoolResult{}, err
	}
	if err := os.MkdirAll(b.StateDir, 0700); err != nil {
		return model.PoolResult{}, err
	}
	lock, err := os.OpenFile(filepath.Join(b.StateDir, ".lvm.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return model.PoolResult{}, err
	}
	defer lock.Close()
	if err := acquireLock(ctx, lock); err != nil {
		return model.PoolResult{}, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return b.ensureLocked(ctx, vgName, devices[0], ownerUID, allowLegacyMigration)
}

func acquireLock(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (b *Backend) ensureLocked(ctx context.Context, vgName string, device model.Device, ownerUID string, allowLegacy bool) (model.PoolResult, error) {
	disk, err := b.identity(ctx, device)
	if err != nil {
		return model.PoolResult{}, err
	}
	pv, vg, err := b.state(ctx, device.Path, vgName)
	if err != nil {
		return model.PoolResult{}, err
	}
	journalPath := filepath.Join(b.StateDir, vgName+".json")
	j, err := readJournal(journalPath)
	if err == nil {
		if j.Identity != device || j.VGName != vgName {
			return model.PoolResult{}, errors.New("pool identity differs from its durable ownership record")
		}
		if j.PoolUID != nil && *j.PoolUID != ownerUID {
			return model.PoolResult{}, errors.New("DiskPool UID does not match the durable ownership record")
		}
		if j.PoolUID == nil && !(allowLegacy && j.Phase == "ready" && pv != nil && vg != nil) {
			return model.PoolResult{}, errors.New("legacy ownership requires a verified existing Ready pool; refusing automatic adoption")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return model.PoolResult{}, fmt.Errorf("cannot read pool ownership journal: %w", err)
	} else {
		if pv != nil || vg != nil {
			return model.PoolResult{}, errors.New("refusing to adopt an existing PV/VG without its ownership journal")
		}
		if err := b.checkReservations(device); err != nil {
			return model.PoolResult{}, err
		}
		if err := b.checkUnused(ctx, disk, device.Path, false); err != nil {
			return model.PoolResult{}, err
		}
		pvUUID, err := newPVUUID()
		if err != nil {
			return model.PoolResult{}, err
		}
		tag, err := randomHex()
		if err != nil {
			return model.PoolResult{}, err
		}
		j = journal{Identity: device, VGName: vgName, PoolUID: &ownerUID, PVUUID: pvUUID, Tag: "diskfleet_" + tag, Phase: "prepared"}
		if err := b.save(journalPath, j); err != nil {
			return model.PoolResult{}, err
		}
	}
	if pv != nil && pv["pv_uuid"] != j.PVUUID {
		return model.PoolResult{}, errors.New("LVM PV UUID does not match the ownership journal")
	}
	if vg != nil {
		if !ownedVG(pv, vg, j) {
			return model.PoolResult{}, errors.New("existing VG ownership or disk membership does not match")
		}
		// Migrate a legacy Ready record only after verifying the actual PV/VG.
		if j.PoolUID == nil || j.Phase != "ready" {
			j.PoolUID, j.Phase = &ownerUID, "ready"
			if err := b.save(journalPath, j); err != nil {
				return model.PoolResult{}, err
			}
		}
		return result(vg, vgName)
	}
	if j.Phase == "ready" {
		return model.PoolResult{}, errors.New("previously managed VG is missing; refusing automatic recreation")
	}
	if pv != nil && pv["vg_name"] != "" {
		return model.PoolResult{}, errors.New("disk belongs to a different VG")
	}
	if pv == nil {
		if j.Phase != "prepared" {
			return model.PoolResult{}, errors.New("previously initialized PV is missing; refusing automatic recreation")
		}
		disk, err = b.identity(ctx, device)
		if err != nil {
			return model.PoolResult{}, err
		}
		if err := b.checkUnused(ctx, disk, device.Path, false); err != nil {
			return model.PoolResult{}, err
		}
		if _, err := b.run(ctx, []string{"pvcreate", "--uuid", j.PVUUID, "--norestorefile", device.Path}); err != nil {
			return model.PoolResult{}, err
		}
		pv, vg, err = b.state(ctx, device.Path, vgName)
		if err != nil {
			return model.PoolResult{}, err
		}
		if pv == nil || pv["pv_uuid"] != j.PVUUID || pv["vg_name"] != "" || vg != nil {
			return model.PoolResult{}, errors.New("PV initialization did not produce the expected isolated PV")
		}
	}
	j.Phase = "pv-created"
	if err := b.save(journalPath, j); err != nil {
		return model.PoolResult{}, err
	}
	disk, err = b.identity(ctx, device)
	if err != nil {
		return model.PoolResult{}, err
	}
	if err := b.checkUnused(ctx, disk, device.Path, true); err != nil {
		return model.PoolResult{}, err
	}
	if _, err := b.run(ctx, []string{"vgcreate", "--addtag", j.Tag, vgName, device.Path}); err != nil {
		return model.PoolResult{}, err
	}
	pv, vg, err = b.state(ctx, device.Path, vgName)
	if err != nil {
		return model.PoolResult{}, err
	}
	if pv == nil || vg == nil || pv["pv_uuid"] != j.PVUUID || !ownedVG(pv, vg, j) {
		return model.PoolResult{}, errors.New("VG creation did not produce the expected owned group")
	}
	j.Phase = "ready"
	if err := b.save(journalPath, j); err != nil {
		return model.PoolResult{}, err
	}
	return result(vg, vgName)
}

func ownedVG(pv, vg map[string]string, j journal) bool {
	return pv != nil && pv["vg_name"] == j.VGName && vg["pv_count"] == "1" && slices.Contains(strings.Split(vg["vg_tags"], ","), j.Tag)
}

func readJournal(name string) (journal, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return journal{}, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return journal{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return journal{}, err
	}
	if _, present := fields["poolUID"]; present && (j.PoolUID == nil || strings.TrimSpace(*j.PoolUID) == "") {
		return journal{}, errors.New("invalid journal DiskPool UID")
	}
	if j.Identity.Path == "" || j.Identity.Serial == "" || j.Identity.SizeBytes <= 0 || j.VGName == "" || j.PVUUID == "" || j.Tag == "" || !slices.Contains([]string{"prepared", "pv-created", "ready"}, j.Phase) {
		return journal{}, errors.New("invalid pool journal identity, phase or ownership")
	}
	return j, nil
}

func (b *Backend) checkReservations(device model.Device) error {
	files, err := filepath.Glob(filepath.Join(b.StateDir, "*.json"))
	if err != nil {
		return err
	}
	for _, name := range files {
		j, err := readJournal(name)
		if err != nil {
			return fmt.Errorf("unreadable ownership journal; manual investigation required: %w", err)
		}
		if j.Identity.Serial == device.Serial || realPath(j.Identity.Path) == realPath(device.Path) {
			return errors.New("disk is already reserved by another pool")
		}
	}
	return nil
}

func (b *Backend) save(name string, j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(b.StateDir, ".journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return err
	}
	dir, err := os.Open(b.StateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (b *Backend) report(ctx context.Context, tool, fields, section string) ([]map[string]string, error) {
	data, err := b.run(ctx, []string{tool, "--reportformat", "json", "--units", "b", "--nosuffix", "--options", fields})
	if err != nil {
		return nil, err
	}
	var report struct {
		Report []map[string]json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Report == nil {
		return nil, fmt.Errorf("invalid or missing %s report", tool)
	}
	rows := []map[string]string{}
	for _, block := range report.Report {
		var values []map[string]json.RawMessage
		if err := json.Unmarshal(block[section], &values); err != nil || values == nil {
			return nil, fmt.Errorf("invalid %s report section", tool)
		}
		for _, value := range values {
			row := map[string]string{}
			for _, field := range strings.Split(fields, ",") {
				raw, present := value[field]
				if !present || string(raw) == "null" {
					return nil, fmt.Errorf("missing %s field %s", tool, field)
				}
				var str string
				if err := json.Unmarshal(raw, &str); err != nil {
					var number json.Number
					if err := json.Unmarshal(raw, &number); err != nil {
						return nil, fmt.Errorf("invalid %s field %s", tool, field)
					}
					str = string(number)
				}
				row[field] = strings.TrimSpace(str)
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (b *Backend) state(ctx context.Context, path, vgName string) (map[string]string, map[string]string, error) {
	pvs, err := b.report(ctx, "pvs", "pv_name,pv_uuid,vg_name", "pv")
	if err != nil {
		return nil, nil, err
	}
	vgs, err := b.report(ctx, "vgs", "vg_name,vg_uuid,vg_tags,vg_size,vg_free,pv_count", "vg")
	if err != nil {
		return nil, nil, err
	}
	var pv, vg map[string]string
	for _, row := range pvs {
		if realPath(row["pv_name"]) == realPath(path) {
			if pv != nil {
				return nil, nil, errors.New("ambiguous LVM PV identity")
			}
			pv = row
		}
	}
	for _, row := range vgs {
		if row["vg_name"] == vgName {
			if vg != nil {
				return nil, nil, errors.New("ambiguous LVM VG identity")
			}
			vg = row
		}
	}
	return pv, vg, nil
}

func validateDevice(path string) error {
	if filepath.Dir(path) != "/dev/disk/by-id" || filepath.Clean(path) != path || partitionPattern.MatchString(filepath.Base(path)) {
		return errors.New("device must be a whole-disk /dev/disk/by-id symlink")
	}
	link, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot resolve block device: %w", err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		return errors.New("stable device path must be a symlink")
	}
	device, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot resolve block device: %w", err)
	}
	if device.Mode()&os.ModeDevice == 0 || device.Mode()&os.ModeCharDevice != 0 {
		return errors.New("stable device path must resolve to a block device")
	}
	return nil
}

// realPath matches Python's non-strict realpath for absent fixture or retained
// device paths. Before a write, validateDevice and inventory independently prove
// that the selected path resolves to a current block device.
func realPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func (b *Backend) identity(ctx context.Context, device model.Device) (model.Disk, error) {
	validator := b.ValidateDevice
	if validator == nil {
		validator = validateDevice
	}
	if err := validator(device.Path); err != nil {
		return model.Disk{}, err
	}
	disks, err := b.Inventory(ctx)
	if err != nil {
		return model.Disk{}, err
	}
	matches := []model.Disk{}
	for _, disk := range disks {
		path := disk.DevicePath
		if path == "" {
			path = disk.Path
		}
		if realPath(path) == realPath(device.Path) {
			matches = append(matches, disk)
		}
	}
	if len(matches) != 1 {
		return model.Disk{}, errors.New("disk missing or device identity is ambiguous")
	}
	disk := matches[0]
	if device.Serial == "" || device.SizeBytes <= 0 || disk.Serial != device.Serial || disk.SizeBytes != device.SizeBytes {
		return model.Disk{}, errors.New("disk serial/capacity changed since approval")
	}
	return disk, nil
}

func (b *Backend) checkUnused(ctx context.Context, disk model.Disk, path string, ownedPV bool) error {
	reasons := []string{}
	for _, reason := range disk.Reasons {
		if !ownedPV || reason != "Filesystem/signature detected: LVM2_member" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) > 0 || (!ownedPV && disk.State != "Available") {
		return fmt.Errorf("disk is protected: %s (%s)", strings.Join(reasons, "; "), disk.State)
	}
	data, err := b.run(ctx, []string{"wipefs", "--no-act", "--json", path})
	if err != nil {
		return err
	}
	var report struct {
		Signatures []struct {
			Type string `json:"type"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Signatures == nil {
		return errors.New("cannot verify disk signatures")
	}
	for _, signature := range report.Signatures {
		if !ownedPV || signature.Type != "LVM2_member" {
			return errors.New("disk contains existing filesystem, RAID or partition-table signatures")
		}
	}
	return nil
}

func randomHex() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	// Match the original uuid.uuid4().hex format, including version/variant bits.
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return hex.EncodeToString(bytes[:]), nil
}

func newPVUUID() (string, error) {
	value, err := randomHex()
	if err != nil {
		return "", err
	}
	return strings.Join([]string{value[:6], value[6:10], value[10:14], value[14:18], value[18:22], value[22:26], value[26:]}, "-"), nil
}

func capacity(value string) (int64, error) {
	// LVM --units b --nosuffix can print decimal byte counts; parse exactly,
	// without losing large-device precision through float64.
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 {
		return 0, errors.New("invalid LVM byte capacity")
	}
	integer := new(big.Int).Quo(r.Num(), r.Denom())
	if !integer.IsInt64() {
		return 0, errors.New("LVM byte capacity overflows int64")
	}
	return integer.Int64(), nil
}

func result(vg map[string]string, name string) (model.PoolResult, error) {
	size, err := capacity(vg["vg_size"])
	if err != nil {
		return model.PoolResult{}, err
	}
	free, err := capacity(vg["vg_free"])
	if err != nil {
		return model.PoolResult{}, err
	}
	if free > size {
		return model.PoolResult{}, errors.New("LVM free capacity exceeds total capacity")
	}
	return model.PoolResult{VGName: name, CapacityBytes: size, AvailableBytes: free}, nil
}
