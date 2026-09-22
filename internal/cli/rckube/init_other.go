//go:build !linux

package rckube

func ensureContainerInit() error { return nil }
