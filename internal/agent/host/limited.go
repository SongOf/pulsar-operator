package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var ErrOutputLimit = errors.New("command output exceeded limit")
var ErrProcessUncertain = errors.New("command process group has not stopped")

// RunLimited bounds both execution and retained output. It never returns command
// stderr, argv, or environment values. A successful return also proves that the
// process group has exited; callers must not remove a device otherwise.
func RunLimited(ctx context.Context, argv []string, timeout time.Duration, outputLimit int) ([]byte, error) {
	if len(argv) == 0 || timeout <= 0 || outputLimit <= 0 || outputLimit > 1<<20 {
		return nil, errors.New("invalid bounded command configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = processAttributes()
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = 2 * time.Second
	output := &limitedOutput{limit: outputLimit, cancel: cancel}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := cmd.Run()
	if cmd.Process != nil {
		// Even an exited launcher must not leave background IO running.
		deadline := time.Now().Add(2 * time.Second)
		for {
			probe := syscall.Kill(-cmd.Process.Pid, 0)
			if errors.Is(probe, syscall.ESRCH) {
				break
			}
			if probe != nil || time.Now().After(deadline) {
				return output.buffer.Bytes(), ErrProcessUncertain
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			time.Sleep(10 * time.Millisecond)
		}
	}
	if output.exceeded {
		return nil, ErrOutputLimit
	}
	if ctx.Err() != nil {
		return output.buffer.Bytes(), ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return output.buffer.Bytes(), fmt.Errorf("command failed (exit %d)", exit.ExitCode())
		}
		return nil, errors.New("command could not execute")
	}
	return output.buffer.Bytes(), nil
}

type limitedOutput struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		w.exceeded = true
		w.cancel()
		return 0, ErrOutputLimit
	}
	return w.buffer.Write(p)
}
