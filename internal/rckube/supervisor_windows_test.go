package rckube

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestWindowsJobStopsDescendants(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	marker := filepath.Join(t.TempDir(), "child.pid")
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	t.Cleanup(supervisor.Shutdown)
	request := processruntime.StartRequest{ID: "tree", UID: "tree-uid", Command: []string{executable, "-test.run=^TestWindowsSubprocess$"}, Environment: map[string]string{"RC_TEST_CHILD": "parent", "RC_TEST_MARKER": marker}}
	state, err := supervisor.Start(request)
	requirements.NoError(err)
	requirements.Equal(phaseRunning, state.Phase)
	requirements.Eventually(func() bool { _, err := os.Stat(marker); return err == nil }, 15*time.Second, 20*time.Millisecond)
	data, err := os.ReadFile(marker)
	requirements.NoError(err)
	childPID, err := strconv.Atoi(string(data))
	requirements.NoError(err)
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(childPID))
	requirements.NoError(err)
	t.Cleanup(func() { _ = windows.CloseHandle(child) })
	state, err = supervisor.Stop(request.ID)
	requirements.NoError(err)
	assert.Equal(t, "Stopped", state.Phase)
	result, err := windows.WaitForSingleObject(child, 5000)
	requirements.NoError(err)
	assert.Equal(t, uint32(windows.WAIT_OBJECT_0), result, "stopping the parent also terminates its descendant")
}

func TestWindowsConPTYAndNamedPipe(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	directory := t.TempDir()
	address := `\\.\pipe\rc-test-` + filepath.Base(directory) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(NewSupervisor(t.TempDir(), 100*time.Millisecond))
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, address) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	client := NewClient(address)
	requirements.Eventually(func() bool { return client.Ping(ctx) == nil }, 10*time.Second, 20*time.Millisecond)
	marker := filepath.Join(directory, "ready.txt")
	request := processruntime.StartRequest{ID: "terminal", UID: "terminal-uid", TTY: true, Command: []string{"powershell.exe", "-NoProfile", "-Command", "[IO.File]::WriteAllText($env:RC_TEST_MARKER, 'ready'); Start-Sleep -Seconds 60"}, Environment: map[string]string{"RC_TEST_MARKER": marker}}
	_, err := client.Start(ctx, request)
	requirements.NoError(err)
	requirements.Eventually(func() bool { _, err := os.Stat(marker); return err == nil }, 15*time.Second, 20*time.Millisecond)
	requirements.NoError(client.Resize(ctx, request.ID, "terminal-client", 30, 100))
	state, err := client.Stop(ctx, request.ID)
	requirements.NoError(err)
	assert.Equal(t, "Stopped", state.Phase)
	_, err = client.Inspect(ctx, "missing")
	assert.ErrorIs(t, err, processruntime.ErrNotFound)
}

// This subprocess fixture creates a real descendant before the supervisor can
// stop it. Its marker is a synchronization point, not a timing assumption.
func TestWindowsSubprocess(t *testing.T) {
	mode := os.Getenv("RC_TEST_CHILD")
	if mode == "" {
		return
	}
	if mode == "parent" {
		executable, err := os.Executable()
		if err != nil {
			panic(err)
		}
		child := exec.Command(executable, "-test.run=^TestWindowsSubprocess$")
		child.Env = append(os.Environ(), "RC_TEST_CHILD=descendant")
		if err := child.Start(); err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Getenv("RC_TEST_MARKER"), []byte(fmt.Sprint(child.Process.Pid)), 0o600); err != nil {
			panic(err)
		}
	}
	time.Sleep(time.Minute)
}

func TestWindowsEnvironmentOverridesAreCaseInsensitive(t *testing.T) {
	// Windows treats Path and PATH as the same key, regardless of host spelling.
	environment, err := mergedEnvironment(map[string]string{"pAtH": "rc-test-path"})
	require.NoError(t, err)
	paths := make([]string, 0)
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "PATH") {
			paths = append(paths, value)
		}
	}
	assert.Equal(t, []string{"rc-test-path"}, paths)
	_, err = mergedEnvironment(map[string]string{"Path": "first", "PATH": "second"})
	require.Error(t, err, "reject ambiguous Windows environment overrides")
}

func TestWindowsAgentCredentialLifetime(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	agentHome := filepath.Join(t.TempDir(), "agent")
	marker := filepath.Join(t.TempDir(), "credential-read")
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	t.Cleanup(supervisor.Shutdown)
	request := processruntime.StartRequest{
		ID: "credential", UID: "credential-uid", AgentHome: agentHome,
		CredentialFiles: map[string][]byte{"agent/auth.json": []byte(`{"test":"fixture"}`)},
		Command:         []string{"powershell.exe", "-NoProfile", "-Command", "Copy-Item (Join-Path $env:RC_TEST_AGENT_HOME 'auth.json') $env:RC_TEST_MARKER; Start-Sleep -Seconds 60"},
		Environment:     map[string]string{"RC_TEST_AGENT_HOME": agentHome, "RC_TEST_MARKER": marker},
	}
	_, err := supervisor.Start(request)
	requirements.NoError(err)
	requirements.Eventually(func() bool {
		data, err := os.ReadFile(marker)
		return err == nil && string(data) == `{"test":"fixture"}`
	}, 15*time.Second, 20*time.Millisecond)
	_, err = supervisor.Stop(request.ID)
	requirements.NoError(err)
	_, err = os.Lstat(filepath.Join(agentHome, "auth.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "remove projected credentials when the process exits")
}
