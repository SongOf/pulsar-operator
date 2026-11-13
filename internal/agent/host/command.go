// Package host supplies bounded, shell-free host command execution.
package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner can be replaced by a fake; tests never need real block devices.
type Runner func(context.Context, []string) ([]byte, error)

func Run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// A nil Stdin reads from the null device. No shell or interactive prompts.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", argv[0], ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[:1000]
		}
		return nil, fmt.Errorf("%s: %w: %s", argv[0], err, message)
	}
	return stdout.Bytes(), nil
}
