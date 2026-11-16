//go:build !linux

package host

import "syscall"

// Non-Linux support is for fixture tests; the production Agent requires Linux.
func processAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
