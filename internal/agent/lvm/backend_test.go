package lvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SongOf/pulsar-operator/internal/agent/discovery"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"golang.org/x/sys/unix"
)

var testDevice = model.Device{Path: "/dev/disk/by-id/wwn-test", Serial: "serial-test", SizeBytes: 4 << 30}

// fakeLVM never invokes commands or accesses any block device. Its state can
// also be serialized by the cross-process locking test.
type fakeLVM struct {
	Disk       model.Disk
	PV         map[string]string
	VG         map[string]string
	Signatures []map[string]string
	Calls      [][]string
	CrashAfter string
}

func newFake() *fakeLVM {
	return &fakeLVM{Disk: model.Disk{Path: testDevice.Path, DevicePath: testDevice.Path, Serial: testDevice.Serial, SizeBytes: testDevice.SizeBytes, State: "Available"}}
}

func (f *fakeLVM) inventory(context.Context) ([]model.Disk, error) {
	disk := f.Disk
	disk.Reasons = slices.Clone(disk.Reasons)
	if f.PV != nil {
		disk.State = "Protected"
		disk.Reasons = append(disk.Reasons, "Filesystem/signature detected: LVM2_member")
	}
	return []model.Disk{disk}, nil
}

func (f *fakeLVM) run(_ context.Context, args []string) ([]byte, error) {
	f.Calls = append(f.Calls, slices.Clone(args))
	switch args[0] {
	case "pvs", "vgs":
		section, row := "pv", f.PV
		if args[0] == "vgs" {
			section, row = "vg", f.VG
		}
		rows := []map[string]string{}
		if row != nil {
			rows = append(rows, row)
		}
		return json.Marshal(map[string]any{"report": []any{map[string]any{section: rows}}})
	case "wipefs":
		signatures := f.Signatures
		if len(signatures) == 0 {
			signatures = []map[string]string{}
			if f.PV != nil {
				signatures = append(signatures, map[string]string{"type": "LVM2_member"})
			}
		}
		return json.Marshal(map[string]any{"signatures": signatures})
	case "pvcreate":
		f.PV = map[string]string{"pv_name": args[len(args)-1], "pv_uuid": args[2], "vg_name": ""}
	case "vgcreate":
		name := args[len(args)-2]
		f.PV["vg_name"] = name
		f.VG = map[string]string{"vg_name": name, "vg_uuid": "vg-test", "vg_tags": args[2], "pv_count": "1", "vg_size": "4290772992", "vg_free": "4290772992"}
	default:
		return nil, fmt.Errorf("unexpected command %v", args)
	}
	if f.CrashAfter == args[0] {
		f.CrashAfter = ""
		return nil, fmt.Errorf("simulated process crash after %s", args[0])
	}
	return nil, nil
}

func (f *fakeLVM) writes() []string {
	result := []string{}
	for _, args := range f.Calls {
		if args[0] == "pvcreate" || args[0] == "vgcreate" {
			result = append(result, args[0])
		}
	}
	return result
}

func fixture(t *testing.T) (*Backend, *fakeLVM) {
	t.Helper()
	f := newFake()
	return &Backend{StateDir: t.TempDir(), Runner: f.run, Inventory: f.inventory, ValidateDevice: func(string) error { return nil }, WritesEnabled: true}, f
}

func ensure(b *Backend) (model.PoolResult, error) {
	return b.EnsurePool(context.Background(), "df-test", []model.Device{testDevice}, true, "uid-pool", false)
}

func requireError(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("expected error containing %q, got %v", message, err)
	}
}

func requireReady(t *testing.T, b *Backend) model.PoolResult {
	t.Helper()
	result, err := ensure(b)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWriteGatesAndInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		enabled, approved bool
		vg, uid           string
		count             int
		message           string
	}{
		{"process gate", false, true, "df-test", "uid", 1, "writes disabled"},
		{"approval gate", true, false, "df-test", "uid", 1, "writes disabled"},
		{"no UID", true, true, "df-test", "", 1, "UID"},
		{"blank UID", true, true, "df-test", "  ", 1, "UID"},
		{"escape", true, true, "../escape", "uid", 1, "volume group"},
		{"option", true, true, "--dangerous", "uid", 1, "volume group"},
		{"reserved name", true, true, "all", "uid", 1, "volume group"},
		{"two disks", true, true, "df-test", "uid", 2, "one disk"},
		{"no disk", true, true, "df-test", "uid", 0, "one disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := fixture(t)
			b.WritesEnabled = tc.enabled
			devices := []model.Device{}
			for range tc.count {
				devices = append(devices, testDevice)
			}
			_, err := b.EnsurePool(context.Background(), tc.vg, devices, tc.approved, tc.uid, false)
			requireError(t, err, tc.message)
			if len(f.Calls) != 0 {
				t.Fatalf("unexpected command before validation: %v", f.Calls)
			}
		})
	}
}

func TestUnapprovedMediaIsNeverWritten(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		mutate        func(*fakeLVM)
	}{
		{"mounted", "protected", func(f *fakeLVM) {
			f.Disk.State = "Protected"
			f.Disk.Reasons = []string{"Mounted device (includes host mounts)"}
		}},
		{"changed serial", "serial/capacity changed", func(f *fakeLVM) { f.Disk.Serial = "replacement" }},
		{"changed capacity", "serial/capacity changed", func(f *fakeLVM) { f.Disk.SizeBytes++ }},
		{"hidden RAID signature", "signatures", func(f *fakeLVM) { f.Signatures = []map[string]string{{"type": "linux_raid_member"}} }},
		{"external PV", "refusing to adopt", func(f *fakeLVM) {
			f.PV = map[string]string{"pv_name": testDevice.Path, "pv_uuid": "external", "vg_name": ""}
		}},
		{"external VG", "refusing to adopt", func(f *fakeLVM) {
			f.VG = map[string]string{"vg_name": "df-test", "vg_uuid": "external", "vg_tags": "", "pv_count": "1", "vg_size": "123", "vg_free": "123"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := fixture(t)
			tc.mutate(f)
			_, err := ensure(b)
			requireError(t, err, tc.message)
			if len(f.writes()) != 0 {
				t.Fatalf("unexpected writes: %v", f.writes())
			}
		})
	}
}

func TestCreateAndIdempotentReconcile(t *testing.T) {
	b, f := fixture(t)
	first := requireReady(t, b)
	if first.VGName != "df-test" || first.CapacityBytes != 4290772992 {
		t.Fatalf("unexpected pool: %+v", first)
	}
	journalPath := filepath.Join(b.StateDir, "df-test.json")
	before, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	f.VG["vg_free"] = "1024.00"
	second := requireReady(t, b)
	if second.AvailableBytes != 1024 {
		t.Fatalf("capacity not refreshed: %+v", second)
	}
	after, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged journal was rewritten")
	}
	if !reflect.DeepEqual(f.writes(), []string{"pvcreate", "vgcreate"}) {
		t.Fatalf("unexpected writes: %v", f.writes())
	}
	for _, args := range f.Calls {
		if slices.Contains(args, "--force") || slices.Contains(args, "--yes") {
			t.Fatalf("unsafe arguments: %v", args)
		}
	}
}

func TestRecoverCrashAfterSuccessfulDiskCommand(t *testing.T) {
	for _, command := range []string{"pvcreate", "vgcreate"} {
		t.Run(command, func(t *testing.T) {
			b, f := fixture(t)
			f.CrashAfter = command
			_, err := ensure(b)
			requireError(t, err, "simulated process crash")
			uuid := f.PV["pv_uuid"]
			// A new Backend represents restarting the process from its journal.
			restarted := *b
			requireReady(t, &restarted)
			if f.PV["pv_uuid"] != uuid || !reflect.DeepEqual(f.writes(), []string{"pvcreate", "vgcreate"}) {
				t.Fatalf("reinitialized after crash: %v", f.writes())
			}
			j, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
			if err != nil || j.Phase != "ready" {
				t.Fatalf("journal not advanced: %+v %v", j, err)
			}
		})
	}
}

func TestOwnedMediaCannotBeReplacedOrRecreated(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		mutate        func(*fakeLVM)
	}{
		{"PV UUID", "UUID", func(f *fakeLVM) { f.PV["pv_uuid"] = "replacement" }},
		{"VG tag", "ownership", func(f *fakeLVM) { f.VG["vg_tags"] = "different-owner" }},
		{"VG membership", "membership", func(f *fakeLVM) { f.VG["pv_count"] = "2" }},
		{"missing VG", "missing", func(f *fakeLVM) { f.VG = nil; f.PV["vg_name"] = "" }},
		{"missing all media", "missing", func(f *fakeLVM) { f.VG = nil; f.PV = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := fixture(t)
			requireReady(t, b)
			tc.mutate(f)
			_, err := ensure(b)
			requireError(t, err, tc.message)
			if len(f.writes()) != 2 {
				t.Fatalf("rewrote owned media: %v", f.writes())
			}
		})
	}
}

func TestUIDBoundBeforeRetryAndDuplicateReservation(t *testing.T) {
	b, f := fixture(t)
	f.CrashAfter = "pvcreate"
	_, err := ensure(b)
	requireError(t, err, "crash")
	_, err = b.EnsurePool(context.Background(), "df-test", []model.Device{testDevice}, true, "replacement-uid", false)
	requireError(t, err, "DiskPool UID")
	f.PV = nil // The prepared journal reserves media even when initialization is absent.
	_, err = b.EnsurePool(context.Background(), "df-second", []model.Device{testDevice}, true, "other-uid", false)
	requireError(t, err, "reserved")
	if len(f.writes()) != 1 {
		t.Fatalf("unexpected writes: %v", f.writes())
	}
}

func rewriteJournal(t *testing.T, b *Backend, change func(map[string]any)) []byte {
	t.Helper()
	name := filepath.Join(b.StateDir, "df-test.json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLegacyReadyMigrationNeedsOwnershipEvidence(t *testing.T) {
	b, f := fixture(t)
	requireReady(t, b)
	before := rewriteJournal(t, b, func(value map[string]any) { delete(value, "poolUID") })
	_, err := ensure(b)
	requireError(t, err, "legacy ownership")
	after, _ := os.ReadFile(filepath.Join(b.StateDir, "df-test.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("journal changed without ownership evidence")
	}
	_, err = b.EnsurePool(context.Background(), "df-test", []model.Device{testDevice}, true, "uid-pool", true)
	if err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
	if err != nil || j.PoolUID == nil || *j.PoolUID != "uid-pool" {
		t.Fatalf("migration did not bind UID: %+v %v", j, err)
	}
	if len(f.writes()) != 2 {
		t.Fatalf("legacy migration initialized media: %v", f.writes())
	}
}

func TestLegacyMigrationRejectsUnprovenMedia(t *testing.T) {
	for _, mutation := range []string{"phase", "PV", "VG tag", "missing VG", "null UID"} {
		t.Run(mutation, func(t *testing.T) {
			b, f := fixture(t)
			requireReady(t, b)
			before := rewriteJournal(t, b, func(value map[string]any) {
				delete(value, "poolUID")
				if mutation == "phase" {
					value["phase"] = "pv-created"
				}
				if mutation == "null UID" {
					value["poolUID"] = nil
				}
			})
			switch mutation {
			case "PV":
				f.PV["pv_uuid"] = "external"
			case "VG tag":
				f.VG["vg_tags"] = "external"
			case "missing VG":
				f.VG = nil
			}
			_, err := b.EnsurePool(context.Background(), "df-test", []model.Device{testDevice}, true, "uid-pool", true)
			if err == nil {
				t.Fatal("unproven legacy ownership accepted")
			}
			after, _ := os.ReadFile(filepath.Join(b.StateDir, "df-test.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("rejected migration changed journal")
			}
			if len(f.writes()) != 2 {
				t.Fatalf("rejected migration wrote media: %v", f.writes())
			}
		})
	}
}

func TestPythonJournalRecovery(t *testing.T) {
	// Literal JSON in Python's original layout, including its UUID grouping.
	const pythonJournal = `{"identity": {"path": "/dev/disk/by-id/wwn-test", "serial": "serial-test", "sizeBytes": 4294967296}, "phase": "prepared", "poolUID": "uid-pool", "pvUUID": "abcdef-0123-4567-89ab-cdef-0123-456789", "tag": "diskfleet_0123456789abcdef0123456789abcdef", "vgName": "df-test"}`
	b, f := fixture(t)
	if err := os.WriteFile(filepath.Join(b.StateDir, "df-test.json"), []byte(pythonJournal), 0600); err != nil {
		t.Fatal(err)
	}
	// Python completed pvcreate and exited before advancing the journal.
	f.PV = map[string]string{"pv_name": testDevice.Path, "pv_uuid": "abcdef-0123-4567-89ab-cdef-0123-456789", "vg_name": ""}
	requireReady(t, b)
	if !reflect.DeepEqual(f.writes(), []string{"vgcreate"}) {
		t.Fatalf("Python PV reinitialized: %v", f.writes())
	}
	j, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Tag != "diskfleet_0123456789abcdef0123456789abcdef" || j.Phase != "ready" {
		t.Fatalf("Python ownership not retained: %+v", j)
	}
}

func TestIdentityIsRecheckedBeforeEachWrite(t *testing.T) {
	for _, replaceAt := range []int{2, 3} {
		t.Run(fmt.Sprint(replaceAt), func(t *testing.T) {
			b, f := fixture(t)
			calls := 0
			b.Inventory = func(ctx context.Context) ([]model.Disk, error) {
				calls++
				if calls == replaceAt {
					f.Disk.Serial = "replaced"
				}
				return f.inventory(ctx)
			}
			_, err := ensure(b)
			requireError(t, err, "serial/capacity changed")
			if len(f.writes()) != replaceAt-2 {
				t.Fatalf("write proceeded after replacement: %v", f.writes())
			}
		})
	}
}

func TestMissingInitializedPVIsNotRecreated(t *testing.T) {
	b, f := fixture(t)
	calls := 0
	b.Inventory = func(ctx context.Context) ([]model.Disk, error) {
		calls++
		if calls == 3 {
			return nil, errors.New("discovery failed")
		}
		return f.inventory(ctx)
	}
	_, err := ensure(b)
	requireError(t, err, "discovery failed")
	j, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
	if err != nil || j.Phase != "pv-created" {
		t.Fatalf("expected initialized journal: %+v %v", j, err)
	}
	f.PV = nil
	b.Inventory = f.inventory
	_, err = ensure(b)
	requireError(t, err, "previously initialized PV is missing")
	if len(f.writes()) != 1 {
		t.Fatalf("missing PV recreated: %v", f.writes())
	}
}

func TestMalformedEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, tool, output string }{
		{"missing report", "pvs", `{}`},
		{"missing section", "pvs", `{"report":[{}]}`},
		{"null rows", "pvs", `{"report":[{"pv":null}]}`},
		{"invalid field", "pvs", `{"report":[{"pv":[{"pv_name":true,"pv_uuid":"a","vg_name":""}]}]}`},
		{"missing signatures", "wipefs", `{}`},
		{"null signatures", "wipefs", `{"signatures":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := fixture(t)
			b.Runner = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == tc.tool {
					return []byte(tc.output), nil
				}
				return f.run(ctx, args)
			}
			if _, err := ensure(b); err == nil {
				t.Fatal("malformed evidence accepted")
			}
			if len(f.writes()) != 0 {
				t.Fatal("wrote despite malformed evidence")
			}
		})
	}
	for _, contents := range []string{`{`, `null`, `{}`, `{"identity":{"path":"a","serial":"b","sizeBytes":10},"vgName":"df-test","pvUUID":"x","tag":"t","phase":"unknown"}`} {
		t.Run("journal "+contents, func(t *testing.T) {
			b, f := fixture(t)
			if err := os.WriteFile(filepath.Join(b.StateDir, "other.json"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := ensure(b)
			requireError(t, err, "unreadable ownership journal")
			if len(f.writes()) != 0 {
				t.Fatal("wrote despite unreadable journal")
			}
		})
	}
}

func TestDeviceValidatorRejectsPathsWithoutBlockAccess(t *testing.T) {
	for _, path := range []string{"/dev/sda", "/dev/disk/by-id/wwn-x-part1", "/tmp/x", "/dev/disk/by-id/../sda", "/dev/disk/by-id/not-a-real-device"} {
		if err := validateDevice(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestCapacityIsExactAndBounded(t *testing.T) {
	value, err := capacity("9007199254740993.00")
	if err != nil || value != 9007199254740993 {
		t.Fatalf("lost capacity precision: %d %v", value, err)
	}
	for _, input := range []string{"NaN", "-1", "9223372036854775808", ""} {
		if _, err := capacity(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestDiscoveryAndBackendRecoverAcrossLVMMetadataChanges(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sysRoot, procRoot, byID := filepath.Join(root, "sys"), filepath.Join(root, "proc"), filepath.Join(root, "by-id")
	for _, name := range []string{byID, filepath.Join(sysRoot, "class/block/vdb/holders"), filepath.Join(sysRoot, "class/block/dm-0/holders")} {
		if err := os.MkdirAll(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(procRoot, "1/mountinfo"), "")
	write(filepath.Join(procRoot, "self/mountinfo"), "")
	write(filepath.Join(procRoot, "swaps"), "Filename Type Size Used Priority\n")
	devicePath := filepath.Join(root, "dev/vdb")
	write(devicePath, "")
	alias := filepath.Join(byID, "virtio-disk-serial")
	if err := os.Symlink(devicePath, alias); err != nil {
		t.Fatal(err)
	}
	device := model.Device{Path: alias, Serial: "serial-test", SizeBytes: 4 << 30}
	f := newFake()
	csiVolume := false
	runner := func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] != "lsblk" {
			return f.run(ctx, args)
		}
		disk := map[string]any{"name": devicePath, "path": devicePath, "kname": "vdb", "type": "disk", "maj:min": "252:16",
			"size": "4294967296", "serial": device.Serial, "wwn": nil, "model": "Test", "ro": "0", "rota": 0,
			"fstype": nil, "pttype": nil, "mountpoints": []any{nil}}
		if f.PV != nil {
			disk["fstype"] = "LVM2_member"
			metadataAlias := filepath.Join(byID, "lvm-pv-uuid-"+f.PV["pv_uuid"])
			if err := os.Symlink(devicePath, metadataAlias); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		if csiVolume {
			childPath := filepath.Join(root, "dev/dm-0")
			disk["children"] = []any{map[string]any{"name": childPath, "path": childPath, "kname": "dm-0", "type": "lvm", "maj:min": "253:0",
				"fstype": "ext4", "pttype": nil, "mountpoints": []any{"/var/lib/kubelet/pods/example"}}}
		}
		return json.Marshal(map[string]any{"blockdevices": []any{disk}})
	}
	d := discovery.Discoverer{Runner: runner, SysRoot: sysRoot, ProcRoot: procRoot, ByID: byID}
	before, err := d.Discover(context.Background())
	if err != nil || len(before) != 1 || before[0].State != "Available" || before[0].Path != alias {
		t.Fatalf("unexpected initial inventory: %+v %v", before, err)
	}
	b := &Backend{StateDir: filepath.Join(root, "state"), Runner: runner, Inventory: d.Discover,
		ValidateDevice: func(string) error { return nil }, WritesEnabled: true}
	claim := func() (model.PoolResult, error) {
		return b.EnsurePool(context.Background(), "df-test", []model.Device{device}, true, "uid-pool", false)
	}
	f.CrashAfter = "pvcreate"
	_, err = claim()
	requireError(t, err, "crash")
	afterPV, err := d.Discover(context.Background())
	if err != nil || afterPV[0].Path != alias || afterPV[0].State != "Protected" {
		t.Fatalf("metadata alias changed identity: %+v %v", afterPV, err)
	}
	if _, err := claim(); err != nil {
		t.Fatalf("cannot resume initialized PV through real discovery: %v", err)
	}
	// CSI later creates and mounts a logical volume. It adds holders and child
	// signatures to discovery, but the already-owned VG must not be recreated.
	csiVolume = true
	write(filepath.Join(root, "dev/dm-0"), "")
	write(filepath.Join(sysRoot, "class/block/vdb/holders/dm-0"), "")
	write(filepath.Join(procRoot, "1/mountinfo"), "21 0 253:0 / /var/lib/kubelet/pods/example rw - ext4 /dev/dm-0 rw\n")
	afterCSI, err := d.Discover(context.Background())
	if err != nil || afterCSI[0].Path != alias || afterCSI[0].State != "Protected" {
		t.Fatalf("unexpected CSI inventory: %+v %v", afterCSI, err)
	}
	if _, err := claim(); err != nil {
		t.Fatalf("owned pool rejected after CSI volume creation: %v", err)
	}
	if !reflect.DeepEqual(f.writes(), []string{"pvcreate", "vgcreate"}) {
		t.Fatalf("recreated owned media: %v", f.writes())
	}
	j, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
	if err != nil || j.Identity != device || j.Phase != "ready" {
		t.Fatalf("journal identity changed: %+v %v", j, err)
	}
}

func TestLVMReportAcceptsNumericFieldsAndWhitespace(t *testing.T) {
	b, _ := fixture(t)
	b.Runner = func(context.Context, []string) ([]byte, error) {
		return []byte(`{"report":[{"vg":[{"vg_name":" df-test ","vg_uuid":" vg-uuid ","vg_tags":" tag ","vg_size":9007199254740993,"vg_free":" 1024.00 ","pv_count":1}]}]}`), nil
	}
	rows, err := b.report(context.Background(), "vgs", "vg_name,vg_uuid,vg_tags,vg_size,vg_free,pv_count", "vg")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["pv_count"] != "1" || rows[0]["vg_name"] != "df-test" {
		t.Fatalf("invalid numeric/whitespace decoding: %+v", rows)
	}
	value, err := result(rows[0], "df-test")
	if err != nil || value.CapacityBytes != 9007199254740993 || value.AvailableBytes != 1024 {
		t.Fatalf("incorrect report values: %+v %v", value, err)
	}
}

func TestWaitingForHostLockCanBeCancelled(t *testing.T) {
	b, f := fixture(t)
	lock, err := os.OpenFile(filepath.Join(b.StateDir, ".lvm.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = b.EnsurePool(ctx, "df-test", []model.Device{testDevice}, true, "uid-pool", false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait ignored cancellation: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatal("commands ran without lock")
	}
}

func TestProcessClaimHelper(t *testing.T) {
	directory := os.Getenv("DISKFLEET_LVM_TEST_DIR")
	if directory == "" {
		return
	}
	statePath := filepath.Join(directory, "fake-lvm-state")
	load := func() (*fakeLVM, error) {
		data, err := os.ReadFile(statePath)
		if err != nil {
			return nil, err
		}
		var f fakeLVM
		err = json.Unmarshal(data, &f)
		return &f, err
	}
	b := &Backend{StateDir: directory, WritesEnabled: true, ValidateDevice: func(string) error { return nil }}
	b.Runner = func(ctx context.Context, args []string) ([]byte, error) {
		f, err := load()
		if err != nil {
			return nil, err
		}
		output, commandErr := f.run(ctx, args)
		data, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(statePath, data, 0600); err != nil {
			return nil, err
		}
		// Ensure the two separate processes overlap while holding .lvm.lock.
		if args[0] == "pvs" {
			time.Sleep(30 * time.Millisecond)
		}
		return output, commandErr
	}
	b.Inventory = func(ctx context.Context) ([]model.Disk, error) {
		f, err := load()
		if err != nil {
			return nil, err
		}
		return f.inventory(ctx)
	}
	_, err := b.EnsurePool(context.Background(), "df-test", []model.Device{testDevice}, true, os.Getenv("DISKFLEET_LVM_TEST_UID"), false)
	status := "Ready"
	if err != nil {
		status = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(status)
	os.Exit(0)
}

func TestTwoProcessesCannotClaimOneVG(t *testing.T) {
	directory := t.TempDir()
	data, err := json.Marshal(newFake())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fake-lvm-state"), data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var outputs [2]bytes.Buffer
	commands := make([]*exec.Cmd, 2)
	for i, uid := range []string{"first-uid", "second-uid"} {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessClaimHelper$")
		command.Env = append(os.Environ(), "DISKFLEET_LVM_TEST_DIR="+directory, "DISKFLEET_LVM_TEST_UID="+uid)
		command.Stdout = &outputs[i]
		command.Stderr = &outputs[i]
		commands[i] = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	statuses := make([]string, 2)
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("process failed: %v %s", err, &outputs[i])
		}
		if err := json.Unmarshal(outputs[i].Bytes(), &statuses[i]); err != nil {
			t.Fatalf("invalid process result: %s", &outputs[i])
		}
	}
	ready := 0
	for _, status := range statuses {
		if status == "Ready" {
			ready++
		} else if !strings.Contains(status, "DiskPool UID") {
			t.Fatalf("unexpected loser result: %v", statuses)
		}
	}
	if ready != 1 {
		t.Fatalf("expected exactly one owner: %v", statuses)
	}
	data, err = os.ReadFile(filepath.Join(directory, "fake-lvm-state"))
	if err != nil {
		t.Fatal(err)
	}
	var f fakeLVM
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.writes(), []string{"pvcreate", "vgcreate"}) {
		t.Fatalf("concurrent media writes: %v", f.writes())
	}
}
