package rckube

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerCancellationClosesIncompleteRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	connection, peer := net.Pipe()
	server := NewServer(NewSupervisor(t.TempDir(), time.Second))
	done := make(chan struct{})
	go func() { defer close(done); server.handle(ctx, connection) }()
	t.Cleanup(func() { _ = peer.Close(); _ = connection.Close(); <-done })
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "server shutdown must unblock an incomplete request")
}
