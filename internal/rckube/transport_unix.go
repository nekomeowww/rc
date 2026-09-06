//go:build !windows

package rckube

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
)

func listenLocal(address string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(address), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(address, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func dialLocal(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", address)
}
