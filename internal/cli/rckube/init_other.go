//go:build !linux

package rckube

func ensureContainerInit() error { return nil }

// addExecutableDirectoryToPath changes nothing: only Linux runtime Pods run an
// injected rc-kube.
func addExecutableDirectoryToPath() error { return nil }
