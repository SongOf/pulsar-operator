package lvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/host"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type benchmarkFake struct {
	base                       *fakeLVM
	lv                         map[string]string
	extra                      []map[string]string
	fioCalls, creates, removes int
	crash                      string
	fioError                   error
	fioErrorOutput             []byte
	invalidMetrics             bool
	before                     func([]string)
	mounted                    bool
}

func benchmarkFixture(t *testing.T) (*Backend, *benchmarkFake, model.BenchmarkRequest) {
	t.Helper()
	b, base := fixture(t)
	requireReady(t, b)
	f := &benchmarkFake{base: base}
	b.Runner = f.run
	return b, f, model.BenchmarkRequest{PoolUID: "uid-pool", NodeUID: "uid-node", VGName: "df-test", RunID: poolstate.RunID("uid-pool"), Device: testDevice, Approved: true}
}

func argument(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, key+"=") {
			return strings.TrimPrefix(a, key+"=")
		}
	}
	return ""
}

func (f *benchmarkFake) run(ctx context.Context, args []string) ([]byte, error) {
	if f.before != nil {
		f.before(args)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch args[0] {
	case "lvs":
		if !slices.Contains(args, "--all") {
			return nil, errors.New("hidden LVs omitted")
		}
		rows := append([]map[string]string{}, f.extra...)
		if f.lv != nil {
			rows = append(rows, f.lv)
		}
		return json.Marshal(map[string]any{"report": []any{map[string]any{"lv": rows}}})
	case "lvcreate":
		f.creates++
		f.lv = map[string]string{"lv_name": argument(args, "--name"), "lv_uuid": "lv-test-uuid", "vg_name": "df-test", "vg_uuid": "vg-test", "lv_tags": argument(args, "--addtag"), "lv_size": "1073741824", "lv_attr": "-wi-a-----", "lv_device_open": "0", "lv_kernel_major": "253", "lv_kernel_minor": "7"}
	case "lsblk":
		mounts := []any{nil}
		if f.mounted {
			mounts = []any{"/business"}
		}
		return json.Marshal(map[string]any{"blockdevices": []any{map[string]any{"name": testDevice.Path, "type": "disk", "fstype": "LVM2_member", "pttype": nil, "mountpoints": []any{nil}, "children": []any{map[string]any{"name": "/dev/dm-7", "type": "lvm", "maj:min": "253:7", "mountpoints": mounts, "fstype": nil, "pttype": nil}}}}})
	case "udevadm":
		if !slices.Equal(args, []string{"udevadm", "settle", "--timeout=10"}) {
			return nil, errors.New("unbounded udev wait")
		}
	case "fio":
		f.fioCalls++
		if !strings.HasPrefix(argument(args, "--filename"), "/dev/disk/by-id/dm-uuid-LVM-") || argument(args, "--thread") != "1" || argument(args, "--allow_file_create") != "0" || argument(args, "--runtime") != "30" || argument(args, "--iodepth") != "16" || argument(args, "--lat_percentiles") != "1" {
			return nil, errors.New("unsafe fio arguments")
		}
		if f.fioError != nil {
			return f.fioErrorOutput, f.fioError
		}
		if f.crash == "fio" {
			panic("fixture process crash")
		}
		if f.invalidMetrics {
			return []byte(`{"jobs":[]}`), nil
		}
		return fioFixture(argument(args, "--name")), nil
	case "lvremove":
		f.removes++
		selector := argument(args, "--select")
		if f.lv != nil && strings.Contains(selector, "lv_uuid="+f.lv["lv_uuid"]+" &&") && strings.Contains(selector, "vg_uuid="+f.lv["vg_uuid"]+" &&") && strings.Contains(selector, "lv_tags="+f.lv["lv_tags"]) {
			f.lv = nil
		}
	default:
		return f.base.run(ctx, args)
	}
	if f.crash == args[0] {
		panic("fixture process crash")
	}
	return nil, nil
}

func fioFixture(name string) []byte {
	io := map[string]any{"io_bytes": int64(4096 * 30000), "bw_bytes": int64(4096000), "iops": 1000.0, "runtime": 30001, "lat_ns": map[string]any{"mean": 1000.0, "percentile": map[string]any{"95.000000": 1500.0, "99.000000": 2000.0}}}
	data, _ := json.Marshal(map[string]any{"jobs": []any{map[string]any{"jobname": name, "error": 0, "read": io, "write": io}}})
	return data
}

func allowBenchmark(context.Context) error { return nil }

func TestBenchmarkSuccessCleanupAndDurableReplay(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || result.Phase != "Succeeded" || !result.CleanupVerified || f.lv != nil || f.fioCalls != 4 || f.creates != 1 || f.removes != 1 {
		t.Fatal(result, err, f)
	}
	pool := &storagev1.DiskPool{ObjectMeta: metav1.ObjectMeta{UID: types.UID(req.PoolUID)}, Spec: storagev1.DiskPoolSpec{Approved: true, VGName: req.VGName, Devices: []storagev1.Device{req.Device}}, Status: storagev1.DiskPoolStatus{Benchmark: &result}}
	if err := poolstate.Reviewable(pool, req.NodeUID); err != nil {
		t.Fatal(err)
	}
	// Reconstructing the Backend models a daemon restart after success.
	restarted := *b
	again, err := restarted.EnsureBenchmark(context.Background(), req, allowBenchmark)
	beforeJSON, _ := json.Marshal(result)
	afterJSON, _ := json.Marshal(again)
	if err != nil || string(beforeJSON) != string(afterJSON) || f.fioCalls != 4 || f.creates != 1 || f.removes != 1 {
		t.Fatal(again, err)
	}
	// Subdirectory journals must not poison the original pool reservation scan.
	if _, err := b.EnsurePool(context.Background(), req.VGName, []model.Device{req.Device}, true, req.PoolUID, false); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkRejectsExistingMediaAndInsufficientCapacity(t *testing.T) {
	for _, scenario := range []string{"business-lv", "capacity", "identity", "disabled", "not-approved", "guard", "mount-protection"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, req := benchmarkFixture(t)
			guard := allowBenchmark
			switch scenario {
			case "business-lv":
				f.extra = []map[string]string{{"lv_name": "business", "lv_uuid": "other", "vg_name": "df-test", "vg_uuid": "vg-test", "lv_tags": "", "lv_size": "1073741824", "lv_attr": "-wi-a-----", "lv_device_open": "1", "lv_kernel_major": "253", "lv_kernel_minor": "2"}}
			case "capacity":
				f.base.VG["vg_free"] = "1024"
			case "identity":
				f.base.PV["pv_uuid"] = "replacement"
			case "disabled":
				b.WritesEnabled = false
			case "not-approved":
				req.Approved = false
			case "guard":
				guard = func(context.Context) error { return errors.New("secret token must never be returned") }
			case "mount-protection":
				f.base.Disk.Reasons = []string{"Mounted device (includes host mounts)"}
			}
			result, err := b.EnsureBenchmark(context.Background(), req, guard)
			if err == nil && result.Phase != "Blocked" && result.Phase != "Failed" {
				t.Fatal(result, err)
			}
			if strings.Contains(fmt.Sprint(err), "secret token") || f.creates != 0 || f.fioCalls != 0 || f.removes != 0 {
				t.Fatal("unsafe command or error", f, err)
			}
		})
	}
}

func TestBenchmarkCrashRecoveryNeverRerunsIO(t *testing.T) {
	for _, stage := range []string{"lvcreate", "fio", "lvremove"} {
		t.Run(stage, func(t *testing.T) {
			b, f, req := benchmarkFixture(t)
			f.crash = stage
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("expected fixture crash")
					}
				}()
				_, _ = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			}()
			f.crash = ""
			calls := f.fioCalls
			result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			if err != nil || result.Phase != "Interrupted" || !result.CleanupVerified || f.lv != nil || f.creates != 1 || f.fioCalls != calls {
				t.Fatal(result, err, f)
			}
			_, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			if err != nil || f.fioCalls != calls {
				t.Fatal("interrupted run replayed IO", err)
			}
		})
	}
}

func TestBenchmarkNeverRemovesReplacementOrOpenLV(t *testing.T) {
	for _, scenario := range []string{"uuid", "tag", "open", "mounted", "foreign-vg", "extra-lv", "during-remove", "moved-vg"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, req := benchmarkFixture(t)
			f.crash = "fio"
			func() {
				defer func() { _ = recover() }()
				_, _ = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			}()
			f.crash = ""
			switch scenario {
			case "uuid":
				f.lv["lv_uuid"] = "replacement"
			case "tag":
				f.lv["lv_tags"] = "other-owner"
			case "open":
				f.lv["lv_device_open"] = "1"
			case "mounted":
				f.mounted = true
			case "foreign-vg":
				f.base.VG["vg_uuid"] = "replacement-vg"
			case "extra-lv":
				other := map[string]string{}
				for k, v := range f.lv {
					other[k] = v
				}
				other["lv_uuid"] = "business"
				f.extra = []map[string]string{other}
			case "during-remove":
				f.before = func(args []string) {
					if args[0] == "lvremove" {
						f.lv["lv_uuid"] = "replacement"
					}
				}
			case "moved-vg":
				f.before = func(args []string) {
					if args[0] == "lvremove" {
						f.lv["vg_name"] = "other-vg"
						f.lv["vg_uuid"] = "other-vg-uuid"
					}
				}
			}
			result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			if err == nil && (result.Phase == "Succeeded" || result.CleanupVerified) {
				t.Fatal(result)
			}
			if f.lv == nil || (scenario != "during-remove" && scenario != "moved-vg" && f.removes != 0) {
				t.Fatal("untrusted LV removed", f)
			}
		})
	}
}

func TestBenchmarkFailuresAreSanitizedAndReplayable(t *testing.T) {
	for _, scenario := range []string{"command", "json", "uncertain-process", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			b, f, req := benchmarkFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "command":
				f.fioError = errors.New("secret-token-value")
			case "json":
				f.invalidMetrics = true
			case "uncertain-process":
				f.fioError = host.ErrProcessUncertain
			case "canceled":
				f.before = func(args []string) {
					if args[0] == "fio" {
						cancel()
					}
				}
			}
			result, err := b.EnsureBenchmark(ctx, req, allowBenchmark)
			if err != nil || result.Phase != "Failed" || strings.Contains(result.Message, "secret-token") {
				t.Fatal(result, err)
			}
			if scenario == "uncertain-process" {
				if f.removes != 0 || result.CleanupVerified {
					t.Fatal("removed while IO unverified")
				}
				return
			}
			if !result.CleanupVerified || f.lv != nil {
				t.Fatal("failed run did not clean own LV", result)
			}
			calls := f.fioCalls
			_, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
			if err != nil || f.fioCalls != calls {
				t.Fatal("failed run repeated", err)
			}
		})
	}
}

func TestBenchmarkCorruptJournalIsNeverReplaced(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil {
		t.Fatal(err)
	}
	path := b.benchmarkPath(req)
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err == nil || f.creates != 1 || f.fioCalls != 4 || result.Phase != "Succeeded" {
		t.Fatal(err)
	}
}

func TestFIORejectsMissingPercentilesAndInvalidMeasurements(t *testing.T) {
	base := fioFixture("seq-write")
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { delete(m, "lat_ns") },
		func(m map[string]any) { m["io_bytes"] = 0 },
		func(m map[string]any) { m["runtime"] = 29999 },
		func(m map[string]any) { m["iops"] = -1 },
		func(m map[string]any) {
			m["lat_ns"].(map[string]any)["percentile"] = map[string]any{"95.000000": 2000, "99.000000": 1000}
		},
	} {
		var doc map[string]any
		_ = json.Unmarshal(base, &doc)
		mutate(doc["jobs"].([]any)[0].(map[string]any)["write"].(map[string]any))
		data, _ := json.Marshal(doc)
		if _, err := parseFIO(data, benchmarkWorkloads[0]); err == nil {
			t.Fatal("invalid metrics accepted")
		}
	}
	if _, err := parseFIO(append(base, []byte(`{}`)...), benchmarkWorkloads[0]); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestBenchmarkHostLockPreventsConcurrentIO(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	first := true
	f.before = func(args []string) {
		if args[0] == "fio" && first {
			first = false
			close(started)
			<-release
		}
	}
	finished := make(chan error, 1)
	go func() {
		result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
		if err == nil && result.Phase != "Succeeded" {
			err = fmt.Errorf("phase %s", result.Phase)
		}
		finished <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := b.EnsureBenchmark(ctx, req, allowBenchmark)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second run acquired active host lock: %v", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.fioCalls != 4 || f.removes != 1 {
		t.Fatal("concurrent benchmark repeated IO", f)
	}
}

func TestBenchmarkGuardRevocationStopsFurtherWorkloads(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	guard := func(context.Context) error {
		if f.fioCalls > 0 {
			return errors.New("authorization revoked with secret")
		}
		return nil
	}
	result, err := b.EnsureBenchmark(context.Background(), req, guard)
	if err != nil || result.Phase != "Failed" || result.ErrorCode != "AuthorizationChanged" || result.CleanupVerified || f.fioCalls != 1 || f.lv == nil || f.removes != 0 {
		t.Fatal(result, err, f)
	}
	result, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || !result.CleanupVerified || result.Phase != "Failed" || f.fioCalls != 1 || f.lv != nil {
		t.Fatal(result, err)
	}
}

func TestBenchmarkFailedFIOPreservesEarlierAndPartialMeasurements(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	f.before = func(args []string) {
		if args[0] == "fio" && argument(args, "--name") == "seq-read" {
			var report map[string]any
			_ = json.Unmarshal(fioFixture("seq-read"), &report)
			job := report["jobs"].([]any)[0].(map[string]any)
			job["error"] = 5
			job["read"].(map[string]any)["runtime"] = 1500
			f.fioErrorOutput, _ = json.Marshal(report)
			f.fioError = errors.New("private stderr")
		}
	}
	result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || result.Phase != "Failed" || !result.CleanupVerified || len(result.Workloads) != 2 {
		t.Fatal(result, err)
	}
	if result.Workloads[0].Name != "seq-write" || result.Workloads[0].Errors != 0 || result.Workloads[1].Name != "seq-read" || result.Workloads[1].Errors != 1 || result.Workloads[1].DurationSeconds != 1.5 {
		t.Fatal(result.Workloads)
	}
	if result.ResultDigest != poolstate.Digest(result) {
		t.Fatal("partial result digest unstable")
	}
	replayed, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || replayed.ResultDigest != result.ResultDigest || len(replayed.Workloads) != 2 || f.fioCalls != 2 {
		t.Fatal(replayed, err)
	}
}

func TestBenchmarkWriteSwitchRevocationDefersCleanup(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	f.before = func(args []string) {
		if args[0] == "fio" {
			b.WritesEnabled = false
		}
	}
	result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || result.Phase != "Failed" || result.CleanupVerified || f.lv == nil || f.removes != 0 || f.fioCalls != 1 {
		t.Fatal(result, err)
	}
	if _, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark); err == nil {
		t.Fatal("disabled writer resumed cleanup")
	}
	b.WritesEnabled = true
	result, err = b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || !result.CleanupVerified || f.lv != nil || f.fioCalls != 1 {
		t.Fatal(result, err)
	}
}

func TestCompletedBenchmarkReplayAllowsLaterBusinessLVs(t *testing.T) {
	b, f, req := benchmarkFixture(t)
	result, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil {
		t.Fatal(err)
	}
	f.extra = []map[string]string{{"lv_name": "business"}}
	f.base.Disk.Reasons = []string{"Mounted device (includes host mounts)"}
	current, err := b.EnsureBenchmark(context.Background(), req, allowBenchmark)
	if err != nil || current.ResultDigest != result.ResultDigest || f.fioCalls != 4 || f.removes != 1 {
		t.Fatal(current, err)
	}
}
