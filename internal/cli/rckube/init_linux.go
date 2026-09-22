//go:build linux

package rckube

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ensureContainerInit gives adopted descendants to Tini when serve is PID 1.
// Re-exec preserves the container PID and signals; the child is no longer PID 1
// and therefore enters serve normally. Existing init systems need no wrapper.
func ensureContainerInit() error {
	if os.Getpid() != 1 {
		return nil
	}
	// NOTICE:
	// PID 1 adopts orphan descendants, but rc-kube only waits for managed commands.
	// Tini reaps adoptees separately so waitpid(-1) cannot consume Cmd.Wait's exit status.
	// Code: internal/rckube/supervisor.go (supervisedProcess.wait) -> internal/osprocess/process_unix.go (waitCommand).
	// Go: https://go.dev/src/os/exec_unix.go and https://go.dev/src/os/pidfd_linux.go
	// Tini: https://github.com/krallin/tini/blob/master/src/tini.c#L510
	// Lifecycle: https://man7.org/linux/man-pages/man2/waitpid.2.html
	// Replace this wrapper only when rc-kube coordinates waits for managed and adopted children.
	initPath, err := exec.LookPath("tini")
	if err != nil {
		return fmt.Errorf("rc-kube serve as PID 1 requires tini in the runner image: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve rc-kube executable for container init: %w", err)
	}
	args := append([]string{initPath, "--", executable}, os.Args[1:]...)
	return syscall.Exec(initPath, args, os.Environ())
}
