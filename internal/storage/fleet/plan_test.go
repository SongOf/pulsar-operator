package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

type fixture struct {
	Pools       []model.Object      `json:"pools"`
	Assignments map[string][]string `json:"assignments"`
	Expected    model.Object        `json:"expected"`
}

func loadFixture(t *testing.T, profile string) fixture {
	t.Helper()
	data, err := os.ReadFile("testdata/" + profile + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

// These full manifests were captured by running the original Python planner.
func TestOriginalPlannerCompatibility(t *testing.T) {
	for _, profile := range []string{"three-node-e2e", "two-node-lab", "two-node-shared-disk-lab"} {
		t.Run(profile, func(t *testing.T) {
			f := loadFixture(t, profile)
			got, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, profile)
			if err != nil {
				t.Fatal(err)
			}
			actual, _ := json.Marshal(got)
			expected, _ := json.Marshal(f.Expected)
			if !bytes.Equal(actual, expected) {
				t.Fatalf("manifest differs from Python:\ngot %s\nwant %s", actual, expected)
			}
		})
	}
}

func TestPlanRejectsUnreviewedOrInsufficientPools(t *testing.T) {
	cases := map[string]func(*fixture){
		"missing UID":           func(f *fixture) { delete(f.Pools[0], "uid") },
		"not Ready":             func(f *fixture) { f.Pools[0]["state"] = "Pending" },
		"missing topology":      func(f *fixture) { delete(f.Pools[0], "topologyHostname") },
		"duplicate pool":        func(f *fixture) { f.Pools = append(f.Pools, f.Pools[0]) },
		"duplicate role entry":  func(f *fixture) { f.Assignments["journal"][1] = f.Assignments["journal"][0] },
		"unknown pool":          func(f *fixture) { f.Assignments["ledger"][0] = "unknown" },
		"missing role":          func(f *fixture) { delete(f.Assignments, "journal") },
		"fewer fault domains":   func(f *fixture) { f.Assignments["ledger"] = f.Assignments["ledger"][:2] },
		"empty intersection":    func(f *fixture) { f.Pools[len(f.Pools)-1]["topologyHostname"] = "outside" },
		"duplicate hostname":    func(f *fixture) { f.Pools[1]["topologyHostname"] = f.Pools[0]["topologyHostname"] },
		"shared journal ledger": func(f *fixture) { f.Assignments["ledger"] = f.Assignments["journal"] },
		"combined capacity":     func(f *fixture) { f.Pools[0]["availableBytes"] = 9 * model.GiB },
		"ledger headroom":       func(f *fixture) { f.Pools[len(f.Pools)-1]["availableBytes"] = 21 * model.GiB },
		"invalid VG regex":      func(f *fixture) { f.Pools[0]["vgName"] = "df_.*" },
		"unreviewed matching VG": func(f *fixture) {
			f.Pools = append(f.Pools, model.Object{"id": "alien", "node": f.Pools[1]["node"], "vgName": f.Pools[0]["vgName"]})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := loadFixture(t, "three-node-e2e")
			change(&f)
			if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, "three-node-e2e"); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
	for _, profile := range []string{"two-node-lab", "two-node-shared-disk-lab"} {
		f := loadFixture(t, profile)
		if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 4, profile); err == nil {
			t.Fatal("two-node replica count accepted")
		}
		f.Pools[0]["availableBytes"] = int64(29) * model.GiB
		if profile == "two-node-shared-disk-lab" {
			f.Pools[0]["availableBytes"] = int64(93) * model.GiB
		}
		if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, profile); err == nil {
			t.Fatal("two-node overcommit accepted")
		}
	}
}

func TestTwoNodeRequiresFreshSchedulableMatchingNodes(t *testing.T) {
	makeNodes := func() []model.Object {
		return []model.Object{
			{"metadata": model.Object{"name": "worker-0", "labels": model.Object{TwoNodeLabel: "true", "kubernetes.io/hostname": "host-0"}}, "status": model.Object{"conditions": []any{model.Object{"type": "Ready", "status": "True"}}}, "spec": model.Object{}},
			{"metadata": model.Object{"name": "worker-1", "labels": model.Object{TwoNodeLabel: "true", "kubernetes.io/hostname": "host-1"}}, "status": model.Object{"conditions": []any{model.Object{"type": "Ready", "status": "True"}}}, "spec": model.Object{}},
		}
	}
	f := loadFixture(t, "two-node-lab")
	if err := ValidateTwoNodeTargets(f.Pools, f.Assignments, makeNodes(), "two-node-lab"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]model.Object) []model.Object{
		"too few":   func(n []model.Object) []model.Object { return n[:1] },
		"duplicate": func(n []model.Object) []model.Object { return []model.Object{n[0], n[0]} },
		"cordoned":  func(n []model.Object) []model.Object { object(n[0]["spec"])["unschedulable"] = true; return n },
		"deleting": func(n []model.Object) []model.Object {
			object(n[0]["metadata"])["deletionTimestamp"] = "2026-01-01T00:00:00Z"
			return n
		},
		"not Ready": func(n []model.Object) []model.Object { object(n[0]["status"])["conditions"] = []any{}; return n },
		"stale hostname": func(n []model.Object) []model.Object {
			object(object(n[0]["metadata"])["labels"])["kubernetes.io/hostname"] = "changed"
			return n
		},
		"wrong node": func(n []model.Object) []model.Object { object(n[0]["metadata"])["name"] = "other"; return n },
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTwoNodeTargets(f.Pools, f.Assignments, change(makeNodes()), "two-node-lab"); err == nil {
				t.Fatal("invalid live nodes accepted")
			}
		})
	}
	if !reflect.DeepEqual(f.Assignments, loadFixture(t, "two-node-lab").Assignments) {
		t.Fatal("validation mutated input")
	}
}

func TestSharedDiskExactCapacityBoundary(t *testing.T) {
	f := loadFixture(t, "two-node-shared-disk-lab")
	for _, pool := range f.Pools {
		pool["availableBytes"] = 94 * model.GiB
	}
	if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, "two-node-shared-disk-lab"); err != nil {
		t.Fatal(err)
	}
	f.Pools[0]["availableBytes"] = 94*model.GiB - 1
	if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, "two-node-shared-disk-lab"); err == nil {
		t.Fatal("accepted capacity below combined requirement")
	}
	f = loadFixture(t, "two-node-lab")
	if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, "two-node-shared-disk-lab"); err == nil {
		t.Fatal("shared profile accepted different role pool sets")
	}
}

func TestPlannerRejectsNamesThatCannotBecomeServices(t *testing.T) {
	f := loadFixture(t, "three-node-e2e")
	for _, name := range []string{"1cluster", "cluster.name"} {
		if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", name, 3, "three-node-e2e"); err == nil {
			t.Fatalf("accepted unusable name %q", name)
		}
	}
}
