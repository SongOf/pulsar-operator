//go:build linux

package host

import "syscall"

func processAttributes() *syscall.SysProcAttr {
	// fio is launched with --thread=1 so killing its process also kills its IO
	// workers if the Agent itself dies before cancellation can run.
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
