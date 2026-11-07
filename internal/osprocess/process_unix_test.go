//go:build !windows

package osprocess

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledWriter blocks its first write until release closes, as a lossless
// reader that falls behind does, and then counts every byte.
type stalledWriter struct {
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	written int
}

func (writer *stalledWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { <-writer.release })
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.written += len(data)
	return len(data), nil
}

func TestWaitKeepsOutputThatASlowWriterHasNotReadYet(t *testing.T) {
	t.Parallel()
	// More than one read of the copy buffer, so output stays in the pipe
	// while the first write is blocked. The pipe holds all of it, so the
	// child exits at once.
	const size = 48 << 10
	command := exec.Command("head", "-c", "49152", "/dev/zero")
	command.WaitDelay = 20 * time.Millisecond
	writer := &stalledWriter{release: make(chan struct{})}
	process, err := Start(command, Options{Output: writer})
	require.NoError(t, err)

	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	// Hold the writer for many times WaitDelay. Wait must not give up on the
	// pipe while the writer, not the pipe, is what it waits for.
	select {
	case err := <-waited:
		close(writer.release)
		require.Failf(t, "Wait returned before output was written", "error: %v, written: %d", err, writer.written)
	case <-time.After(25 * command.WaitDelay):
		close(writer.release)
	}
	assert.NoError(t, <-waited, "the child exited cleanly and its output was read to the end")
	require.Equal(t, size, writer.written, "a slow writer receives every byte the child wrote")
}

func TestWaitStopsReadingAPipeThatADescendantKeepsOpen(t *testing.T) {
	t.Parallel()
	command := exec.Command("sh", "-c", "sleep 30 & echo started")
	command.WaitDelay = 50 * time.Millisecond
	writer := &stalledWriter{release: make(chan struct{})}
	close(writer.release)
	process, err := Start(command, Options{Output: writer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Stop(true) })

	started := time.Now()
	err = process.Wait()
	require.ErrorIs(t, err, exec.ErrWaitDelay, "report that the pipe was closed before end of file")
	require.Less(t, time.Since(started), 10*time.Second, "an idle pipe held open by a descendant does not stall Wait")
	require.Equal(t, len("started\n"), writer.written)
}
