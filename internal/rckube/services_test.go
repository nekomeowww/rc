//go:build !windows

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rckube

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const (
	testServiceName     = "agent"
	testServiceEndpoint = "/run/rc/test.sock"
	testServiceHelper   = "RC_TEST_SERVICE"
	testServiceTouch    = "touch"
	testServiceVariable = "MODE"
	testServiceValue    = "service"
)

// lockedBuffer is a service log that tests read while services write.
type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.String()
}

// serviceRuns returns the recorded runs of one service, newest first.
func serviceRuns(t *testing.T, supervisor *Supervisor) []processruntime.HistoryEntry {
	t.Helper()
	entries, err := supervisor.List()
	require.NoError(t, err)
	runs := make([]processruntime.HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, serviceIDPrefix+testServiceName+"_") {
			runs = append(runs, entry)
		}
	}
	return runs
}

func newServiceSupervisor(t *testing.T, log *lockedBuffer, services ...processruntime.Service) *Supervisor {
	t.Helper()
	supervisor := newTestSupervisor(t, t.TempDir(), 100*time.Millisecond, WithRoots(t.TempDir(), t.TempDir(), t.TempDir()), WithServices(services, log))
	supervisor.serviceBackoff = serviceBackoff{
		initial: 20 * time.Millisecond,
		max:     80 * time.Millisecond,
		reset:   time.Hour,
	}
	return supervisor
}

func TestServiceRestartsWithCappedBackoffWithoutActivity(t *testing.T) {
	t.Parallel()
	log := new(lockedBuffer)
	supervisor := newServiceSupervisor(t, log, processruntime.Service{
		Name:    testServiceName,
		Command: []string{"sh", "-c", "printf run; exit 3"},
	})
	ctx, cancel := context.WithCancel(t.Context())
	wait := supervisor.runServices(ctx, testServiceEndpoint)

	require.Eventually(t, func() bool {
		return strings.Count(log.String(), "restarting in") >= 5
	}, 5*time.Second, 10*time.Millisecond, "restart the service each time it exits")
	cancel()
	wait()

	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	assert.Equal(t, []string{
		"rc-kube: service agent exited with code 3; restarting in 20ms",
		"rc-kube: service agent exited with code 3; restarting in 40ms",
		"rc-kube: service agent exited with code 3; restarting in 80ms",
		"rc-kube: service agent exited with code 3; restarting in 80ms",
	}, lines[:4], "double the delay up to the cap")
	assert.True(t, supervisor.LastActivity().IsZero(), "starting or restarting a service is not activity")
	runs := serviceRuns(t, supervisor)
	assert.LessOrEqual(t, len(runs), serviceRunHistory+1, "bound the runs kept per service")
	var transcript bytes.Buffer
	require.NoError(t, supervisor.Logs(runs[0].ID, &transcript))
	assert.Equal(t, "run", transcript.String(), "service output stays inspectable as a process transcript")
}

func TestServiceBackoffResetsAfterALongRun(t *testing.T) {
	t.Parallel()
	log := new(lockedBuffer)
	supervisor := newServiceSupervisor(t, log, processruntime.Service{
		Name:    testServiceName,
		Command: []string{"sh", "-c", "exit 1"},
	})
	supervisor.serviceBackoff.reset = 0
	ctx, cancel := context.WithCancel(t.Context())
	wait := supervisor.runServices(ctx, testServiceEndpoint)
	require.Eventually(t, func() bool {
		return strings.Count(log.String(), "restarting in") >= 3
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	wait()

	assert.NotContains(t, log.String(), "restarting in 40ms", "a run that lasted long enough restarts at the initial delay")
}

func TestServiceReceivesWorkspaceEnvironmentAndEndpoint(t *testing.T) {
	t.Parallel()
	supervisor := newServiceSupervisor(t, new(lockedBuffer), processruntime.Service{
		Name:        testServiceName,
		Command:     []string{"sh", "-c", `printf '%s|%s|%s' "$RC_KUBE_SOCKET" "$MODE" "$(pwd -P)"; exec sleep 30`},
		Environment: map[string]string{testServiceVariable: testServiceValue},
	})
	ctx, cancel := context.WithCancel(t.Context())
	wait := supervisor.runServices(ctx, testServiceEndpoint)
	defer func() {
		cancel()
		wait()
	}()

	workspace, err := os.Stat(supervisor.workspaceDir)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		runs := serviceRuns(t, supervisor)
		if len(runs) != 1 {
			return false
		}
		var transcript bytes.Buffer
		if supervisor.Logs(runs[0].ID, &transcript) != nil {
			return false
		}
		parts := strings.Split(transcript.String(), "|")
		if len(parts) != 3 || parts[0] != testServiceEndpoint || parts[1] != testServiceValue {
			return false
		}
		directory, err := os.Stat(parts[2])
		return err == nil && os.SameFile(workspace, directory)
	}, 3*time.Second, 10*time.Millisecond, "the service runs in the Workspace directory with its env and the endpoint")
}

func TestServicesStopOnShutdownAndAreNotRestarted(t *testing.T) {
	t.Parallel()
	log := new(lockedBuffer)
	supervisor := newServiceSupervisor(t, log, processruntime.Service{
		Name:    testServiceName,
		Command: []string{"sh", "-c", "exec sleep 30"},
	})
	ctx, cancel := context.WithCancel(t.Context())
	wait := supervisor.runServices(ctx, testServiceEndpoint)
	require.Eventually(t, func() bool {
		runs := serviceRuns(t, supervisor)
		return len(runs) == 1 && runs[0].Phase == phaseRunning
	}, 3*time.Second, 10*time.Millisecond)

	cancel()
	stopped := make(chan struct{})
	go func() {
		wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("services did not stop within the stop grace")
	}

	runs := serviceRuns(t, supervisor)
	require.Len(t, runs, 1, "a stopped service is not started again")
	assert.Equal(t, phaseStopped, runs[0].Phase)
	assert.Empty(t, log.String(), "shutdown is not a restart")
}

func TestServeStartsServicesThatReportWork(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	require.NoError(t, err)
	supervisor, client := startTestServer(t, WithServices([]processruntime.Service{
		{
			Name:        testServiceName,
			Command:     []string{executable, "-test.run=^TestServiceHelperProcess$"},
			Environment: map[string]string{testServiceHelper: testServiceTouch},
		},
	}, nil))

	require.Eventually(t, func() bool {
		last, err := client.LastActivity(t.Context())
		return err == nil && !last.IsZero()
	}, 5*time.Second, 20*time.Millisecond, "a service reports work through RC_KUBE_SOCKET")
	runs := serviceRuns(t, supervisor)
	require.NotEmpty(t, runs)
	assert.Equal(t, phaseRunning, runs[0].Phase, "the service keeps running after it reports work")
}

// TestServiceHelperProcess is a resident service started by
// TestServeStartsServicesThatReportWork. It reports work, then waits to be
// stopped.
func TestServiceHelperProcess(t *testing.T) {
	if os.Getenv(testServiceHelper) != testServiceTouch {
		t.Skip("helper process for TestServeStartsServicesThatReportWork")
	}
	require.NoError(t, NewClient(os.Getenv(EndpointVariable)).Touch(context.Background()))
	time.Sleep(time.Minute)
}

//nolint:paralleltest // Changes the process environment.
func TestPrepareServicesResolvesAndRemovesContainerVariables(t *testing.T) {
	source := processruntime.ServiceEnvironmentPrefix + "0"
	t.Setenv(source, "synthetic-token")
	t.Setenv(processruntime.ServiceEnvironmentPrefix+"1", "unused")
	encoded, err := processruntime.EncodeServices([]processruntime.Service{
		{
			Name:            testServiceName,
			Command:         []string{"device-agent"},
			Environment:     map[string]string{testServiceVariable: testServiceValue},
			EnvironmentFrom: map[string]string{"TOKEN": source, "OPTIONAL": processruntime.ServiceEnvironmentPrefix + "9"},
		},
	})
	require.NoError(t, err)

	services, err := PrepareServices(encoded)
	require.NoError(t, err)

	require.Len(t, services, 1)
	assert.Equal(t, map[string]string{testServiceVariable: testServiceValue, "TOKEN": "synthetic-token"}, services[0].Environment, "an unset optional reference stays unset")
	assert.Nil(t, services[0].EnvironmentFrom)
	_, found := os.LookupEnv(source)
	assert.False(t, found, "no other process inherits a service value")
	_, found = os.LookupEnv(processruntime.ServiceEnvironmentPrefix + "1")
	assert.False(t, found)

	empty, err := PrepareServices("")
	require.NoError(t, err)
	assert.Empty(t, empty)
}
