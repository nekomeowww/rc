//go:build linux

package rckube

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	runtime "github.com/nekomeowww/rc/internal/rckube"
)

// ensureContainerInit gives adopted descendants to Tini when serve is PID 1.
// Re-exec preserves the container PID and signals; the child is no longer PID 1
// and therefore enters serve normally. Existing init systems need no wrapper.
func ensureContainerInit() error {
	if os.Getpid() != 1 {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve rc-kube executable for container init: %w", err)
	}
	// An image with its own init system names it in RC_KUBE_INIT. The init
	// system receives the path of this rc-kube and starts serve with it.
	if hook := os.Getenv(runtime.InitVariable); hook != "" {
		hookPath, err := exec.LookPath(hook)
		if err != nil {
			return fmt.Errorf("find the %s program %s: %w", runtime.InitVariable, hook, err)
		}
		args := append([]string{hookPath, executable}, os.Args[1:]...)
		return syscall.Exec(hookPath, args, os.Environ())
	}
	// An image built before rc-kube was injected can start serve through an
	// rc-kube script on PATH. Keep its start sequence.
	if wrapper := runtime.ImageServeWrapper(os.Getenv("PATH"), executable); wrapper != "" {
		args := append([]string{wrapper}, os.Args[1:]...)
		return syscall.Exec(wrapper, args, os.Environ())
	}
	// NOTICE:
	// PID 1 adopts orphan descendants, but rc-kube only waits for managed commands.
	// Tini reaps adoptees separately so waitpid(-1) cannot consume Cmd.Wait's exit status.
	// Code: internal/rckube/supervisor.go (supervisedProcess.wait) -> internal/osprocess/process_unix.go (waitCommand).
	// Go: https://go.dev/src/os/exec_unix.go and https://go.dev/src/os/pidfd_linux.go
	// Tini: https://github.com/krallin/tini/blob/master/src/tini.c#L510
	// Lifecycle: https://man7.org/linux/man-pages/man2/waitpid.2.html
	// Replace this wrapper only when rc-kube coordinates waits for managed and adopted children.
	initPath, err := runtime.FindTini(executable)
	if err != nil {
		return err
	}
	args := append([]string{initPath, "--", executable}, os.Args[1:]...)
	return syscall.Exec(initPath, args, os.Environ())
}

// addExecutableDirectoryToPath puts the directory of this rc-kube on PATH for
// every process it starts. The controller injects rc-kube into a directory
// that the image does not know, and processes such as a service that runs
// "rc-kube process touch" must reach this rc-kube.
func addExecutableDirectoryToPath() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve rc-kube executable for PATH: %w", err)
	}

	return os.Setenv("PATH", runtime.WithDirectoryInPath(os.Getenv("PATH"), filepath.Dir(executable)))
}
