package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLimitedHelper(t *testing.T) {
	if os.Getenv("DISKFLEET_LIMITED_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "success":
		fmt.Print(`{"ok":true}`)
	case "large":
		fmt.Print(strings.Repeat("x", 4096))
	case "secret":
		fmt.Print(`{"partial":true}`)
		fmt.Fprint(os.Stderr, "token=never-expose-this-fixture")
		os.Exit(7)
	case "wait":
		for {
			time.Sleep(time.Second)
		}
	}
	os.Exit(0)
}

func TestRunLimitedBoundsOutputAndHidesErrors(t *testing.T) {
	t.Setenv("DISKFLEET_LIMITED_HELPER", "1")
	for _, mode := range []string{"success", "large", "secret", "wait"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 5 * time.Second
			if mode == "wait" {
				timeout = 50 * time.Millisecond
			}
			out, err := RunLimited(context.Background(), []string{os.Args[0], "-test.run=^TestLimitedHelper$", mode}, timeout, 1024)
			if strings.Contains(fmt.Sprint(err), "token=") || strings.Contains(string(out), "never-expose") {
				t.Fatal("stderr exposed")
			}
			switch mode {
			case "success":
				if err != nil || string(out) != `{"ok":true}` {
					t.Fatal(string(out), err)
				}
			case "large":
				if !errors.Is(err, ErrOutputLimit) {
					t.Fatal(err)
				}
			case "secret":
				if err == nil || !strings.Contains(err.Error(), "exit 7") || string(out) != `{"partial":true}` {
					t.Fatal(err)
				}
			case "wait":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
		})
	}
}
