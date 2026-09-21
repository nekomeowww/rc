//go:build linux

package rckube

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ensureContainerInit gives adopted descendants to Tini when serve is PID 1.
// The supervisor still waits for its own commands through os/exec. A second
// wait(-1) loop in that same process could consume their exit statuses.
// Re-exec preserves the container PID and signals; the child is no longer PID 1
// and therefore enters serve normally. Existing init systems need no wrapper.
func ensureContainerInit() error {
	if os.Getpid() != 1 {
		return nil
	}
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
