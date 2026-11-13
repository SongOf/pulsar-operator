package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/agent/model"
)

type fixture struct {
	t       *testing.T
	root    string
	d       Discoverer
	devices []any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir()}
	f.d = Discoverer{SysRoot: filepath.Join(f.root, "sys"), ProcRoot: filepath.Join(f.root, "proc"), ByID: filepath.Join(f.root, "by-id")}
	f.mkdir(f.d.ByID)
	f.write(filepath.Join(f.d.ProcRoot, "1/mountinfo"), "")
	f.write(filepath.Join(f.d.ProcRoot, "self/mountinfo"), "")
	f.write(filepath.Join(f.d.ProcRoot, "swaps"), "Filename Type Size Used Priority\n")
	f.d.Runner = func(ctx context.Context, argv []string) ([]byte, error) {
		t.Helper()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(argv, []string{"lsblk", "--json", "--bytes", "--paths", "--output", "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,FSTYPE,PTTYPE,MOUNTPOINTS,RO,ROTA"}) {
			t.Fatalf("unexpected discovery command: %q", argv)
		}
		return json.Marshal(map[string]any{"blockdevices": f.devices})
	}
	return f
}

func (f *fixture) mkdir(path string) {
	f.t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write(path, value string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) alias(name, target string) string {
	f.t.Helper()
	path := filepath.Join(f.d.ByID, name)
	if err := os.Symlink(target, path); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) disk(name, number string) map[string]any {
	f.t.Helper()
	path := filepath.Join(f.root, "dev", name)
	f.write(path, "")
	f.mkdir(filepath.Join(f.d.SysRoot, "class/block", name, "holders"))
	disk := map[string]any{"name": path, "kname": name, "path": path, "type": "disk", "size": int64(4 * 1024 * 1024 * 1024), "serial": "serial-" + name,
		"maj:min": number, "ro": false, "rota": true, "mountpoints": []any{nil}, "fstype": nil, "pttype": nil}
	f.devices = append(f.devices, disk)
	return disk
}

func (f *fixture) find() []model.Disk {
	f.t.Helper()
	disks, err := f.d.Discover(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return disks
}

func assertProtected(t *testing.T, disk model.Disk, reason string) {
	t.Helper()
	if disk.State != "Protected" || !strings.Contains(strings.Join(disk.Reasons, "\n"), reason) {
		t.Fatalf("expected protection %q, got %+v", reason, disk)
	}
}

func TestHardwareIdentitySurvivesLVMInitialization(t *testing.T) {
	f := newFixture(t)
	disk := f.disk("vdb", "252:16")
	disk["serial"] = " disk-serial "
	disk["model"] = "  Test Model  "
	alias := f.alias("virtio-disk-serial", disk["path"].(string))
	before := f.find()[0]
	if before.State != "Available" || before.Path != alias || before.Serial != "disk-serial" || before.Model != "Test Model" || before.PoolID != nil || before.Reasons == nil {
		t.Fatalf("unexpected fresh disk: %+v", before)
	}
	f.alias("lvm-pv-uuid-new-pv", disk["path"].(string))
	disk["fstype"] = "LVM2_member"
	f.write(filepath.Join(f.d.SysRoot, "class/block/vdb/holders/dm-0"), "")
	after := f.find()[0]
	if before.ID != after.ID || before.Path != after.Path || before.DevicePath != after.DevicePath || before.Serial != after.Serial || before.SizeBytes != after.SizeBytes {
		t.Fatalf("identity changed during LVM initialization: before=%+v after=%+v", before, after)
	}
	assertProtected(t, after, "Filesystem/signature detected: LVM2_member")
	assertProtected(t, after, "Device has active holders")
}

func TestHardwareAliasPriorityAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		aliases []string
		want    string
	}{
		{"wwn preferred", []string{"virtio-disk", "wwn-physical", "lvm-pv-uuid-new", "wwn-physical-part1"}, "wwn-physical"},
		{"metadata excluded", []string{"lvm-pv-uuid-new"}, ""},
		{"partition excluded", []string{"wwn-disk-part12"}, ""},
		{"lexical hardware preference", []string{"virtio-b", "ata-a"}, "ata-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			for _, name := range tc.aliases {
				f.alias(name, disk["path"].(string))
			}
			found := f.find()[0]
			if tc.want == "" {
				assertProtected(t, found, "Stable /dev/disk/by-id path is missing")
			} else if found.Path != filepath.Join(f.d.ByID, tc.want) || found.State != "Available" {
				t.Fatalf("unexpected alias selection: %+v", found)
			}
		})
	}
}

func TestLSBLKFlagRepresentations(t *testing.T) {
	for _, value := range []any{false, 0, "0", "false", "FALSE"} {
		t.Run(stringValue(value), func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			f.alias("wwn-vdb", disk["path"].(string))
			disk["ro"], disk["rota"], disk["size"] = value, value, "4294967296"
			found := f.find()[0]
			if found.State != "Available" || found.Media != "SSD" || found.SizeBytes != 4294967296 {
				t.Fatalf("incorrect string/boolean/number decoding: %+v", found)
			}
		})
	}
	for _, value := range []any{true, 1, "1", "true"} {
		t.Run("readonly-"+stringValue(value), func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			disk["ro"] = value
			assertProtected(t, f.find()[0], "Read-only device")
		})
	}
	for _, value := range []any{nil, "", "no", 2, []any{false}} {
		t.Run("unknown-"+stringValue(value), func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			disk["ro"] = value
			assertProtected(t, f.find()[0], "Cannot verify device read-only state")
		})
	}
}

func stringValue(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestProtectHostMountsPartitionsSwapAndHolders(t *testing.T) {
	f := newFixture(t)
	root := f.disk("sda", "8:0")
	partition := f.disk("sda1", "8:1")
	partition["type"], partition["fstype"], partition["pttype"] = "part", "ext4", "gpt"
	root["children"] = []any{partition}
	holders := f.disk("sdb", "8:16")
	swap := f.disk("sdc", "8:32")
	free := f.disk("sdd", "8:48")
	selfMounted := f.disk("sde", "8:64")
	for _, disk := range []map[string]any{root, holders, swap, free, selfMounted} {
		f.alias("wwn-"+disk["kname"].(string), disk["path"].(string))
	}
	f.write(filepath.Join(f.d.ProcRoot, "1/mountinfo"), "21 0 8:1 / / rw - ext4 /dev/sda1 rw\n")
	f.write(filepath.Join(f.d.ProcRoot, "self/mountinfo"), "22 0 8:64 / /data rw shared:10 - ext4 /dev/sde rw\n")
	f.write(filepath.Join(f.d.ProcRoot, "swaps"), "Filename Type Size Used Priority\n"+swap["path"].(string)+" partition 123 0 -2\n")
	f.write(filepath.Join(f.d.SysRoot, "class/block/sdb/holders/md0"), "")
	disks := f.find()
	if len(disks) != 5 {
		t.Fatalf("partitions must not become independent candidates: %+v", disks)
	}
	assertProtected(t, disks[0], "Has partitions")
	assertProtected(t, disks[0], "Mounted device (includes host mounts)")
	assertProtected(t, disks[0], "Filesystem/signature detected: ext4")
	assertProtected(t, disks[0], "Partition table detected: gpt")
	assertProtected(t, disks[1], "Device has active holders")
	assertProtected(t, disks[2], "Swap device")
	if disks[3].State != "Available" {
		t.Fatalf("blank disk should remain available: %+v", disks[3])
	}
	assertProtected(t, disks[4], "Mounted device (includes host mounts)")
}

func TestHostSwapfileNeedNotExistInContainerMountNamespace(t *testing.T) {
	f := newFixture(t)
	root := f.disk("sda", "8:0")
	free := f.disk("sdb", "8:16")
	for _, disk := range []map[string]any{root, free} {
		f.alias("wwn-"+disk["kname"].(string), disk["path"].(string))
	}
	f.write(filepath.Join(f.d.ProcRoot, "1/mountinfo"), "21 0 8:0 / / rw - ext4 /dev/sda rw\n")
	missing := filepath.Join(f.root, "host-only-swap.img")
	f.write(filepath.Join(f.d.ProcRoot, "swaps"), "Filename Type Size Used Priority\n"+missing+" file 123 0 -2\n")
	disks := f.find()
	assertProtected(t, disks[0], "Mounted device")
	if disks[1].State != "Available" {
		t.Fatalf("host-only swapfile blocked unrelated blank disk: %+v", disks[1])
	}
	// An unresolved block-device swap still invalidates the safety snapshot.
	f.write(filepath.Join(f.d.ProcRoot, "swaps"), "Filename Type Size Used Priority\n"+missing+" partition 123 0 -2\n")
	assertProtected(t, f.find()[1], "Cannot read host mount/swap state")
}

func TestMissingOrMalformedEvidenceProtectsDisks(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*fixture, map[string]any)
	}{
		{"missing host mounts", "Cannot read host mount/swap state", func(f *fixture, _ map[string]any) { remove(f.t, filepath.Join(f.d.ProcRoot, "1/mountinfo")) }},
		{"missing swaps", "Cannot read host mount/swap state", func(f *fixture, _ map[string]any) { remove(f.t, filepath.Join(f.d.ProcRoot, "swaps")) }},
		{"malformed mountinfo", "Cannot read host mount/swap state", func(f *fixture, _ map[string]any) {
			f.write(filepath.Join(f.d.ProcRoot, "self/mountinfo"), "truncated")
		}},
		{"malformed swaps", "Cannot read host mount/swap state", func(f *fixture, _ map[string]any) { f.write(filepath.Join(f.d.ProcRoot, "swaps"), "") }},
		{"missing holders", "Cannot verify device holders", func(f *fixture, _ map[string]any) { remove(f.t, filepath.Join(f.d.SysRoot, "class/block/vdb/holders")) }},
		{"missing number", "Cannot verify device major/minor number", func(_ *fixture, disk map[string]any) { delete(disk, "maj:min") }},
		{"missing serial", "Stable serial number is missing", func(_ *fixture, disk map[string]any) { disk["serial"] = nil }},
		{"missing size", "Device is smaller than 64 MiB", func(_ *fixture, disk map[string]any) { disk["size"] = nil }},
		{"small size", "Device is smaller than 64 MiB", func(_ *fixture, disk map[string]any) { disk["size"] = 64*1024*1024 - 1 }},
		{"missing read only", "Cannot verify device read-only state", func(_ *fixture, disk map[string]any) { delete(disk, "ro") }},
		{"missing signature column", "Cannot verify filesystem signatures", func(_ *fixture, disk map[string]any) { delete(disk, "fstype") }},
		{"missing partition column", "Cannot verify partition table", func(_ *fixture, disk map[string]any) { delete(disk, "pttype") }},
		{"missing mount column", "Cannot verify device mountpoints", func(_ *fixture, disk map[string]any) { delete(disk, "mountpoints") }},
		{"vanished device", "Cannot resolve device path", func(f *fixture, disk map[string]any) { remove(f.t, disk["path"].(string)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			f.alias("wwn-vdb", disk["path"].(string))
			tc.change(f, disk)
			assertProtected(t, f.find()[0], tc.reason)
		})
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateObservationsCannotHideProtection(t *testing.T) {
	f := newFixture(t)
	disk := f.disk("vdb", "252:16")
	f.alias("wwn-vdb", disk["path"].(string))
	duplicate := make(map[string]any)
	for key, value := range disk {
		duplicate[key] = value
	}
	duplicate["fstype"] = "ext4"
	duplicate["mountpoints"] = []any{"/mounted", nil}
	f.devices = append(f.devices, duplicate, duplicate)
	found := f.find()
	if len(found) != 1 {
		t.Fatalf("expected a single physical disk: %+v", found)
	}
	assertProtected(t, found[0], "Filesystem/signature detected: ext4")
	assertProtected(t, found[0], "Mounted device")
	if len(found[0].Reasons) != 2 {
		t.Fatalf("duplicate reasons: %v", found[0].Reasons)
	}
	duplicate["serial"] = "replacement-disk"
	assertProtected(t, f.find()[0], "Conflicting observations for device")
}

func TestMalformedLSBLKAndRunnerFailures(t *testing.T) {
	for _, data := range []string{"[]", "null", "{}", `{"blockdevices":null}`, `{"blockdevices":[null]}`, `{"blockdevices":[{"type":"disk","children":{}}]}`, `{"blockdevices":[]} {}`, `{"blockdevices":[{"type":false}]}`} {
		t.Run(data, func(t *testing.T) {
			f := newFixture(t)
			f.d.Runner = func(context.Context, []string) ([]byte, error) { return []byte(data), nil }
			if _, err := f.d.Discover(context.Background()); err == nil {
				t.Fatalf("accepted malformed lsblk output: %s", data)
			}
		})
	}
	for _, value := range []any{true, "-1.5", "18446744073709551616", map[string]any{"bytes": 100}} {
		t.Run("size-"+stringValue(value), func(t *testing.T) {
			f := newFixture(t)
			disk := f.disk("vdb", "252:16")
			disk["size"] = value
			if _, err := f.d.Discover(context.Background()); err == nil {
				t.Fatalf("accepted invalid byte capacity: %v", value)
			}
		})
	}
	f := newFixture(t)
	f.d.Runner = func(context.Context, []string) ([]byte, error) { return nil, context.DeadlineExceeded }
	if _, err := f.d.Discover(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runner failure was lost: %v", err)
	}
}
