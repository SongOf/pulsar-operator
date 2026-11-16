package lvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/host"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Benchmark journals deliberately live below benchmarks/, outside the legacy
// ownership journal glob. UUIDs and nonce tags authorize recovery, never names.
type benchmarkJournal struct {
	Version int                         `json:"version"`
	Stage   string                      `json:"stage"`
	PVUUID  string                      `json:"pvUUID"`
	VGUUID  string                      `json:"vgUUID"`
	VGTag   string                      `json:"vgTag"`
	LVName  string                      `json:"lvName"`
	LVTag   string                      `json:"lvTag"`
	LVUUID  string                      `json:"lvUUID,omitempty"`
	Result  storagev1.DiskPoolBenchmark `json:"result"`
}

var lvmID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
var benchmarkName = regexp.MustCompile(`^dfbench_[0-9a-f]{32}$`)

// EnsureBenchmark is a blocking, per-node serialized operation. Call it from a
// worker separate from discovery. No command supplied by an API caller is used.
// Terminal runs are replayed; an interrupted run is cleaned, never rerun.
func (b *Backend) EnsureBenchmark(ctx context.Context, req model.BenchmarkRequest, guard func(context.Context) error) (storagev1.DiskPoolBenchmark, error) {
	var empty storagev1.DiskPoolBenchmark
	if !b.WritesEnabled || !req.Approved || req.PoolUID == "" || req.NodeUID == "" || req.RunID != poolstate.RunID(req.PoolUID) || !vgPattern.MatchString(req.VGName) || b.StateDir == "" || b.Inventory == nil || guard == nil {
		return empty, errors.New("benchmark approval, identity, inventory and live guard are required")
	}
	if err := os.MkdirAll(b.StateDir, 0700); err != nil {
		return empty, errors.New("benchmark state directory unavailable")
	}
	lock, err := os.OpenFile(filepath.Join(b.StateDir, ".lvm.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return empty, errors.New("benchmark host lock unavailable")
	}
	defer lock.Close()
	if err := acquireLock(ctx, lock); err != nil {
		return empty, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := guard(ctx); err != nil {
		return empty, errors.New("benchmark live authorization rejected")
	}

	// Use the bounded runner for every LVM command in this workflow as well as
	// fio. The existing initialization journal implementation remains unchanged.
	bounded := *b
	bounded.Runner = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "lvcreate" || args[0] == "lvremove" || args[0] == "fio" {
			if !b.WritesEnabled || !req.Approved {
				return nil, errors.New("benchmark disk writes disabled")
			}
			if err := guard(ctx); err != nil {
				return nil, errors.New("benchmark live authorization rejected")
			}
		}
		if args[0] == "lvs" {
			args = append(slices.Clone(args), "--all", "--config", "report/binary_values_as_numeric=1")
		}
		if b.Runner != nil {
			return b.Runner(ctx, args)
		}
		timeout := 30 * time.Second
		if args[0] == "fio" {
			timeout = 45 * time.Second
		}
		return host.RunLimited(ctx, args, timeout, 1<<20)
	}
	path := b.benchmarkPath(req)
	data, err := readBenchmarkData(path)
	if err == nil {
		var j benchmarkJournal
		if len(data) > 1<<20 || json.Unmarshal(data, &j) != nil || !validBenchmarkJournal(j, req) {
			return empty, errors.New("invalid benchmark recovery journal")
		}
		if _, _, err := bounded.verifyBenchmarkOwner(ctx, req, &j); err != nil {
			return empty, err
		}
		if j.Stage == "complete" {
			return j.Result, nil
		}
		// The old process has released the host lock. UUID/tag and device-open
		// checks below still forbid deleting an LV held by an orphan IO process.
		if j.Result.Phase == "Running" || j.Result.Phase == "Succeeded" {
			j.Result.Phase = "Interrupted"
			j.Result.ErrorCode = "AgentInterrupted"
		}
		return bounded.finishBenchmark(ctx, req, path, &j, true)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return empty, errors.New("benchmark recovery journal unavailable")
	}
	owner, vg, err := bounded.verifyBenchmarkOwner(ctx, req, nil)
	if err != nil {
		return empty, err
	}
	nonce, err := randomHex()
	if err != nil {
		return empty, errors.New("benchmark identity generation failed")
	}
	now := benchmarkNow()
	j := benchmarkJournal{Version: 1, Stage: "planned", PVUUID: owner.PVUUID, VGUUID: vg["vg_uuid"], VGTag: owner.Tag,
		LVName: "dfbench_" + nonce, LVTag: "dfbench_" + nonce,
		Result: storagev1.DiskPoolBenchmark{PoolUID: req.PoolUID, NodeUID: req.NodeUID, VGName: req.VGName, Device: req.Device, RunID: req.RunID, Profile: poolstate.Profile, Phase: "Running", StartedAt: &now, TestSizeBytes: poolstate.TestSizeBytes},
	}
	if err := saveBenchmark(path, j); err != nil {
		return empty, err
	}
	lvs, err := bounded.benchmarkLVs(ctx, req.VGName)
	if err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "InventoryUnavailable", false)
	}
	if len(lvs) != 0 {
		return bounded.failBenchmark(ctx, req, path, &j, "ExistingLogicalVolumes", false)
	}
	disk, err := bounded.identity(ctx, req.Device)
	if err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "DeviceProtected", false)
	}
	if err := bounded.checkUnused(ctx, disk, req.Device.Path, true); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "DeviceProtected", false)
	}
	free, err := capacity(vg["vg_free"])
	if err != nil || free < poolstate.TestSizeBytes {
		j.Result.Phase, j.Result.ErrorCode = "Blocked", "InsufficientCapacity"
		return bounded.finishBenchmark(ctx, req, path, &j, true)
	}
	if err := guard(ctx); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "AuthorizationChanged", true)
	}
	if _, _, err := bounded.verifyBenchmarkOwner(ctx, req, &j); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "IdentityChanged", false)
	}
	// Recheck the empty VG immediately before allocation; the Agent lock is
	// not a lock respected by CSI or an administrator's LVM commands.
	lvs, err = bounded.benchmarkLVs(ctx, req.VGName)
	if err != nil || len(lvs) != 0 {
		return bounded.failBenchmark(ctx, req, path, &j, "ExistingLogicalVolumes", false)
	}
	if _, err := bounded.run(ctx, []string{"lvcreate", "--yes", "--zero", "n", "--wipesignatures", "n", "--size", "1073741824B", "--name", j.LVName, "--addtag", j.LVTag, req.VGName}); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "LVCreateFailed", !errors.Is(err, host.ErrProcessUncertain))
	}
	// The host udev daemon creates the immutable dm-uuid symlink. LVM metadata
	// may already be committed while that userspace event is still pending.
	if _, err := bounded.run(ctx, []string{"udevadm", "settle", "--timeout=10"}); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "DeviceEventsUnsettled", true)
	}
	if _, err := bounded.findBenchmarkLV(ctx, req, &j, false); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "LVOwnershipUnverified", false)
	}
	j.Stage = "created"
	if err := saveBenchmark(path, j); err != nil {
		return empty, err
	}
	for _, workload := range benchmarkWorkloads {
		if err := guard(ctx); err != nil {
			return bounded.failBenchmark(ctx, req, path, &j, "AuthorizationChanged", true)
		}
		if _, _, err := bounded.verifyBenchmarkOwner(ctx, req, &j); err != nil {
			return bounded.failBenchmark(ctx, req, path, &j, "IdentityChanged", false)
		}
		if _, err := bounded.findBenchmarkLV(ctx, req, &j, false); err != nil {
			return bounded.failBenchmark(ctx, req, path, &j, "LVOwnershipUnverified", false)
		}
		j.Stage = "running"
		if err := saveBenchmark(path, j); err != nil {
			return empty, err
		}
		// A name containing both immutable LVM UUIDs cannot resolve to a new,
		// same-name LV. fio is prohibited from creating a missing target file.
		device := "/dev/disk/by-id/dm-uuid-LVM-" + strings.ReplaceAll(j.VGUUID, "-", "") + strings.ReplaceAll(j.LVUUID, "-", "")
		args := fioArguments(device, workload)
		output, err := bounded.run(ctx, args)
		if err != nil {
			if sample, parseErr := parseFIOSample(output, workload); parseErr == nil {
				j.Result.Workloads = append(j.Result.Workloads, sample)
			}
			return bounded.failBenchmark(ctx, req, path, &j, "FIOFailed", !errors.Is(err, host.ErrProcessUncertain))
		}
		metric, err := parseFIO(output, workload)
		if err != nil {
			if sample, parseErr := parseFIOSample(output, workload); parseErr == nil {
				j.Result.Workloads = append(j.Result.Workloads, sample)
			}
			return bounded.failBenchmark(ctx, req, path, &j, "InvalidFIOResult", true)
		}
		j.Result.Workloads = append(j.Result.Workloads, metric)
		if err := saveBenchmark(path, j); err != nil {
			return empty, err
		}
	}
	j.Stage = "measured"
	if err := saveBenchmark(path, j); err != nil {
		return empty, err
	}
	if err := guard(ctx); err != nil {
		return bounded.failBenchmark(ctx, req, path, &j, "AuthorizationChanged", true)
	}
	j.Result.Phase = "Succeeded"
	return bounded.finishBenchmark(ctx, req, path, &j, true)
}

func (b *Backend) benchmarkPath(req model.BenchmarkRequest) string {
	sum := sha256.Sum256([]byte(req.PoolUID))
	return filepath.Join(b.StateDir, "benchmarks", hex.EncodeToString(sum[:]), req.RunID+".json")
}

func readBenchmarkData(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, (1<<20)+1))
}

func validBenchmarkJournal(j benchmarkJournal, req model.BenchmarkRequest) bool {
	r := j.Result
	if j.Stage == "complete" && (!slices.Contains([]string{"Succeeded", "Failed", "Blocked", "Interrupted"}, r.Phase) || !r.CleanupVerified || r.CompletedAt == nil || r.CompletedAt.IsZero()) {
		return false
	}
	return j.Version == 1 && slices.Contains([]string{"planned", "created", "running", "measured", "cleanup-pending", "complete"}, j.Stage) &&
		r.PoolUID == req.PoolUID && r.NodeUID == req.NodeUID && r.VGName == req.VGName && r.Device == req.Device && r.RunID == req.RunID && r.Profile == poolstate.Profile && r.TestSizeBytes == poolstate.TestSizeBytes &&
		lvmID.MatchString(j.PVUUID) && lvmID.MatchString(j.VGUUID) && benchmarkName.MatchString(j.LVName) && j.LVTag == j.LVName &&
		(j.LVUUID == "" || lvmID.MatchString(j.LVUUID)) && r.StartedAt != nil && (j.Stage != "complete" || (r.ResultDigest != "" && r.ResultDigest == poolstate.Digest(r)))
}

func saveBenchmark(path string, j benchmarkJournal) error {
	data, err := json.Marshal(j)
	if err != nil || len(data) > 1<<20 {
		return errors.New("benchmark journal encoding failed")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("benchmark journal directory unavailable")
	}
	// Persist new benchmarks/ and pool-directory entries before a disk command
	// can rely on this record surviving a machine crash.
	for _, parent := range []string{filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		d, err := os.Open(parent)
		if err != nil {
			return errors.New("benchmark journal parent unavailable")
		}
		err = d.Sync()
		closeErr := d.Close()
		if err != nil || closeErr != nil {
			return errors.New("benchmark journal parent sync failed")
		}
	}
	f, err := os.CreateTemp(dir, ".benchmark-")
	if err != nil {
		return errors.New("benchmark journal create failed")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err != nil {
		return errors.New("benchmark journal write failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("benchmark journal close failed")
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return errors.New("benchmark journal rename failed")
	}
	d, err := os.Open(dir)
	if err != nil {
		return errors.New("benchmark journal directory unavailable")
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return errors.New("benchmark journal sync failed")
	}
	return nil
}

func (b *Backend) verifyBenchmarkOwner(ctx context.Context, req model.BenchmarkRequest, saved *benchmarkJournal) (journal, map[string]string, error) {
	var empty journal
	if _, err := b.identity(ctx, req.Device); err != nil {
		return empty, nil, errors.New("benchmark device identity unverified")
	}
	j, err := readJournal(filepath.Join(b.StateDir, req.VGName+".json"))
	if err != nil || j.PoolUID == nil || *j.PoolUID != req.PoolUID || j.Identity != req.Device || j.VGName != req.VGName || j.Phase != "ready" {
		return empty, nil, errors.New("benchmark pool ownership unverified")
	}
	pv, vg, err := b.state(ctx, req.Device.Path, req.VGName)
	if err != nil || pv == nil || vg == nil || pv["pv_uuid"] != j.PVUUID || !ownedVG(pv, vg, j) || !lvmID.MatchString(vg["vg_uuid"]) {
		return empty, nil, errors.New("benchmark PV/VG identity unverified")
	}
	if saved != nil && (saved.PVUUID != j.PVUUID || saved.VGUUID != vg["vg_uuid"] || saved.VGTag != j.Tag) {
		return empty, nil, errors.New("benchmark PV/VG identity changed")
	}
	return j, vg, nil
}

func (b *Backend) benchmarkLVs(ctx context.Context, vgName string) ([]map[string]string, error) {
	rows, err := b.benchmarkAllLVs(ctx)
	if err != nil {
		return nil, errors.New("benchmark LV inventory unverified")
	}
	result := []map[string]string{}
	for _, row := range rows {
		if row["vg_name"] == vgName {
			result = append(result, row)
		}
	}
	return result, nil
}

func (b *Backend) benchmarkAllLVs(ctx context.Context) ([]map[string]string, error) {
	return b.report(ctx, "lvs", "lv_name,lv_uuid,vg_name,vg_uuid,lv_tags,lv_size,lv_attr,lv_device_open,lv_kernel_major,lv_kernel_minor", "lv")
}

func (b *Backend) benchmarkLVAbsent(ctx context.Context, req model.BenchmarkRequest, j *benchmarkJournal) bool {
	if _, _, err := b.verifyBenchmarkOwner(ctx, req, j); err != nil {
		return false
	}
	rows, err := b.benchmarkAllLVs(ctx)
	if err != nil {
		return false
	}
	for _, lv := range rows {
		if lv["vg_name"] == req.VGName || (j.LVUUID != "" && lv["lv_uuid"] == j.LVUUID) || slices.Contains(strings.Split(lv["lv_tags"], ","), j.LVTag) {
			return false
		}
	}
	return true
}

// A planned journal may recover the LV UUID from its unique creation tag. Save
// that UUID before any fio or removal; a replacement with the same name fails.
func (b *Backend) findBenchmarkLV(ctx context.Context, req model.BenchmarkRequest, j *benchmarkJournal, allowAbsent bool) (map[string]string, error) {
	rows, err := b.benchmarkLVs(ctx, req.VGName)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && allowAbsent {
		if b.benchmarkLVAbsent(ctx, req, j) {
			return nil, nil
		}
		return nil, errors.New("temporary LV absence is unverified")
	}
	if len(rows) != 1 {
		return nil, errors.New("benchmark requires an exclusive temporary LV")
	}
	lv := rows[0]
	size, err := capacity(lv["lv_size"])
	if err != nil || size != poolstate.TestSizeBytes || lv["lv_name"] != j.LVName || lv["vg_uuid"] != j.VGUUID || !slices.Contains(strings.Split(lv["lv_tags"], ","), j.LVTag) || !lvmID.MatchString(lv["lv_uuid"]) || len(lv["lv_attr"]) < 6 || lv["lv_attr"][0] != '-' || lv["lv_device_open"] != "0" {
		return nil, errors.New("temporary LV identity or exclusive access unverified")
	}
	if j.LVUUID != "" && j.LVUUID != lv["lv_uuid"] {
		return nil, errors.New("temporary LV UUID changed")
	}
	if lv["lv_attr"][4] == 'a' {
		if err := b.verifyBenchmarkTree(ctx, req, lv); err != nil {
			return nil, err
		}
	} else if !allowAbsent {
		return nil, errors.New("temporary LV is not active")
	}
	j.LVUUID = lv["lv_uuid"]
	return lv, nil
}

func (b *Backend) failBenchmark(ctx context.Context, req model.BenchmarkRequest, path string, j *benchmarkJournal, code string, cleanup bool) (storagev1.DiskPoolBenchmark, error) {
	j.Result.Phase, j.Result.ErrorCode = "Failed", code
	if code == "ExistingLogicalVolumes" {
		j.Result.Phase = "Blocked"
		j.Result.CleanupVerified = true
		j.Stage = "complete"
		now := benchmarkNow()
		j.Result.CompletedAt = &now
		j.Result.Message = "Existing logical volumes prevent automatic benchmarking."
		j.Result.ResultDigest = poolstate.Digest(j.Result)
		return j.Result, saveBenchmark(path, *j)
	}
	return b.finishBenchmark(ctx, req, path, j, cleanup)
}

func (b *Backend) finishBenchmark(ctx context.Context, req model.BenchmarkRequest, path string, j *benchmarkJournal, safeToCleanup bool) (storagev1.DiskPoolBenchmark, error) {
	j.Stage = "cleanup-pending"
	if err := saveBenchmark(path, *j); err != nil {
		return j.Result, err
	}
	if safeToCleanup {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, _, err := b.verifyBenchmarkOwner(cleanup, req, j); err == nil {
			lv, err := b.findBenchmarkLV(cleanup, req, j, true)
			if err == nil && lv != nil {
				if err = saveBenchmark(path, *j); err == nil {
					// The selector is evaluated by LVM under its own metadata lock.
					// A same-name replacement cannot be selected for destruction.
					_, err = b.run(cleanup, []string{"lvremove", "--yes", "--select", "lv_uuid=" + j.LVUUID + " && vg_uuid=" + j.VGUUID + " && lv_tags=" + j.LVTag})
				}
			}
			if err == nil {
				j.Result.CleanupVerified = b.benchmarkLVAbsent(cleanup, req, j)
			}
		}
	}
	if !j.Result.CleanupVerified {
		j.Result.Phase = "Failed"
		if j.Result.ErrorCode == "" {
			j.Result.ErrorCode = "CleanupUnverified"
		}
		j.Result.Message = "Temporary LV cleanup is not verified; allocation remains blocked."
	} else {
		j.Stage = "complete"
		j.Result.Message = "Benchmark finished; temporary LV cleanup verified."
	}
	now := benchmarkNow()
	j.Result.CompletedAt = &now
	j.Result.ResultDigest = poolstate.Digest(j.Result)
	return j.Result, saveBenchmark(path, *j)
}

type benchmarkWorkload struct {
	name, rw string
	block    int64
}

func benchmarkNow() metav1.Time { return metav1.NewTime(time.Now().UTC().Truncate(time.Second)) }

var benchmarkWorkloads = []benchmarkWorkload{{"seq-write", "write", 1 << 20}, {"seq-read", "read", 1 << 20}, {"rand-write", "randwrite", 4096}, {"rand-read", "randread", 4096}}

func fioArguments(device string, w benchmarkWorkload) []string {
	return []string{"fio", "--name=" + w.name, "--filename=" + device, "--allow_file_create=0", "--ioengine=libaio", "--thread=1", "--numjobs=1", "--direct=1", "--rw=" + w.rw, fmt.Sprintf("--bs=%d", w.block), "--iodepth=16", "--size=1073741824", "--runtime=30", "--time_based=1", "--lat_percentiles=1", "--percentile_list=95:99", "--output-format=json", "--eta=never", "--exitall_on_error=1"}
}
