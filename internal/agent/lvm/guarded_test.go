package lvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"golang.org/x/sys/unix"
)

func ensureGuarded(b *Backend, guard func(context.Context) error) (model.PoolResult, error) {
	return b.EnsurePoolGuarded(context.Background(), "df-test", []model.Device{testDevice}, true, "uid-pool", false, guard)
}

func TestPoolAuthorizationIsRecheckedAfterWaitingForHostLock(t *testing.T) {
	for _, scenario := range []string{"approval-revoked", "node-deleted", "pool-replaced"} {
		t.Run(scenario, func(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var revoked atomic.Bool
			var checks atomic.Int32
			validated := make(chan struct{})
			guard := func(context.Context) error {
				denied := revoked.Load()
				if checks.Add(1) == 1 {
					close(validated)
				}
				if denied {
					return errors.New(scenario + ": secret Kubernetes response")
				}
				return nil
			}
			done := make(chan error, 1)
			go func() {
				_, err := b.EnsurePoolGuarded(ctx, "df-test", []model.Device{testDevice}, true, "uid-pool", false, guard)
				done <- err
			}()
			select {
			case <-validated:
			case <-ctx.Done():
				t.Fatal("initializer did not validate before waiting for the host lock")
			}
			// The external lock still owns the entire device critical section.
			// A live authorization change must be observed after it is released.
			revoked.Store(true)
			if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal("initializer did not finish after releasing the host lock")
			}
			if err == nil || err.Error() != "pool live authorization rejected" || checks.Load() < 2 {
				t.Fatalf("post-lock authorization was not enforced or leaked details: checks=%d error=%v", checks.Load(), err)
			}
			if len(f.Calls) != 0 {
				t.Fatalf("commands ran after live authorization was revoked: %v", f.Calls)
			}
			if _, err := os.Stat(filepath.Join(b.StateDir, "df-test.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected lock waiter reserved media: %v", err)
			}
		})
	}
}

func TestPoolAuthorizationIsRecheckedBeforeEachDeviceWrite(t *testing.T) {
	for _, scenario := range []string{"before-pvcreate", "before-vgcreate", "writes-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			b, f := fixture(t)
			revoked := false
			guard := func(context.Context) error {
				if revoked {
					return errors.New("secret authorization backend response")
				}
				return nil
			}
			b.Runner = func(ctx context.Context, args []string) ([]byte, error) {
				output, err := f.run(ctx, args)
				if args[0] == "pvcreate" && scenario != "before-pvcreate" {
					if scenario == "writes-disabled" {
						b.WritesEnabled = false
					} else {
						revoked = true
					}
				}
				if args[0] == "wipefs" && scenario == "before-pvcreate" {
					// Revoke after the durable intent exists and the final media
					// probe returns, immediately before the first destructive step.
					if _, err := os.Stat(filepath.Join(b.StateDir, "df-test.json")); err == nil {
						revoked = true
					}
				}
				return output, err
			}
			_, err := ensureGuarded(b, guard)
			message := "pool live authorization rejected"
			if scenario == "writes-disabled" {
				message = "disk writes disabled"
			}
			requireError(t, err, message)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("authorization failure leaked backend details")
			}
			want, phase := []string{}, "prepared"
			if scenario != "before-pvcreate" {
				want, phase = []string{"pvcreate"}, "pv-created"
			}
			if !reflect.DeepEqual(f.writes(), want) {
				t.Fatalf("device write occurred after authorization changed: %v", f.writes())
			}
			journal, err := readJournal(filepath.Join(b.StateDir, "df-test.json"))
			if err != nil || journal.Phase != phase {
				t.Fatalf("interrupted initialization lost its recoverable intent: %+v, %v", journal, err)
			}
			uuid := journal.PVUUID
			// Restoring the same authorization resumes the original ownership
			// record without repeating a PV initialization that already succeeded.
			revoked, b.WritesEnabled, b.Runner = false, true, f.run
			if _, err := ensureGuarded(b, guard); err != nil {
				t.Fatal(err)
			}
			if f.PV["pv_uuid"] != uuid || !reflect.DeepEqual(f.writes(), []string{"pvcreate", "vgcreate"}) {
				t.Fatalf("authorized recovery changed media identity or repeated a write: %v", f.writes())
			}
		})
	}
}

func TestGuardedPoolRequiresLiveGuardEvenForReadyOwnership(t *testing.T) {
	b, f := fixture(t)
	if _, err := ensureGuarded(b, nil); err == nil || len(f.Calls) != 0 {
		t.Fatal("missing guard was accepted")
	}
	requireReady(t, b)
	f.Calls = nil
	_, err := ensureGuarded(b, func(context.Context) error { return errors.New("approval no longer matches") })
	if err == nil || err.Error() != "pool live authorization rejected" || len(f.Calls) != 0 {
		t.Fatalf("Ready ownership bypassed live authorization: commands=%v error=%v", f.Calls, err)
	}
}
