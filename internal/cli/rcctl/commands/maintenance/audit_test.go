package maintenance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/audit"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const pruneCommand = "prune"

type auditFixtureOptions struct {
	edit func(map[string]json.RawMessage)
}

// auditFixture runs the real command tree against a read-only Kubernetes HTTP
// fixture. An explicit kubeconfig prevents any access to the caller's cluster.
func auditFixture(t *testing.T) func(...string) (string, error) {
	t.Helper()
	return configuredAuditFixture(t, auditFixtureOptions{})
}

func configuredAuditFixture(t *testing.T, options auditFixtureOptions) func(...string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "audit.json"))
	require.NoError(t, err)
	var responses map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &responses))
	if options.edit != nil {
		options.edit(responses)
	}
	var mutex sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("read-only audit attempted %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body, ok := responses[r.URL.Path]
		if !ok {
			t.Errorf("unexpected fixture request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, err := w.Write(body)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	config := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(config, []byte(`apiVersion: v1
kind: Config
clusters:
- name: fixture
  cluster:
    server: `+server.URL+`
contexts:
- name: fixture
  context:
    cluster: fixture
    namespace: audit
current-context: fixture
`), 0600))
	return func(args ...string) (string, error) {
		cmd := &cobra.Command{Use: "rcctl", SilenceErrors: true, SilenceUsage: true}
		flags := kubeconfig.NewFlags()
		flags.AddFlags(cmd.PersistentFlags())
		cmd.AddCommand(newDoctorCommand(flags), newPruneCommand(flags))
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append([]string{"--kubeconfig", config}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
}

func TestDoctorReportsEvidence(t *testing.T) {
	run := auditFixture(t)
	out, err := run(doctorCommand, "-o", "json")
	require.NoError(t, err)
	var report audit.Report
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.True(t, report.Inventory.Complete)
	assert.EqualValues(t, 40*1024*1024*1024, report.Summary.PVCRequestedBytes)
	for _, code := range []string{"ProvisioningFailure", "TerminalRuntimePod", "PVCConflict", "TerminalHistory"} {
		assert.Contains(t, out, code)
	}
	out, err = run(doctorCommand)
	require.NoError(t, err)
	for _, finding := range report.Findings {
		assert.Contains(t, out, finding.Code)
	}
	// Lifecycle deadlines are shown as published, never recomputed.
	assert.Contains(t, out, "idle-suspend-at=2026-01-03T00:00:00Z")
	assert.Contains(t, out, "delete-at=none")
}

// TestPruneReportsBacklogReadOnly relies on the fixture server failing every
// non-GET request: prune must never delete history itself.
func TestPruneReportsBacklogReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, retention, history string
		retentionState, status   string
		pending                  *int32
	}{
		{"unpublished", "", "", audit.RetentionUnset, audit.HistoryStatusUnpublished, nil},
		{"stale", "", `{"retained":3,"pendingCleanup":0,"observedGeneration":1}`, audit.RetentionUnset, audit.HistoryStatusStale, new(int32(0))},
		{"configured", `{"ttlAfterFinished":"24h"}`, `{"retained":0,"pendingCleanup":1,"effectiveTTL":"24h0m0s","effectiveMaxEntries":100,"observedGeneration":2}`, audit.RetentionConfigured, audit.HistoryStatusCurrent, new(int32(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := configuredAuditFixture(t, auditFixtureOptions{edit: func(responses map[string]json.RawMessage) {
				editWorkspace(t, responses, func(workspace map[string]any) {
					workspace["metadata"].(map[string]any)["generation"] = 2
					if tc.retention != "" {
						workspace["spec"].(map[string]any)["executionRetention"] = decode(t, tc.retention)
					}
					if tc.history != "" {
						workspace["status"].(map[string]any)["executionHistory"] = decode(t, tc.history)
					}
				})
			}})
			out, err := run(pruneCommand, "-o", "json")
			require.NoError(t, err)
			var report audit.HistoryReport
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			assert.True(t, report.Complete)
			require.Len(t, report.Targets, 1)
			target := report.Targets[0]
			assert.Equal(t, "Workspace", target.Target.Kind)
			assert.Equal(t, "dev", target.Target.Name)
			assert.Equal(t, tc.retentionState, target.Retention)
			assert.Equal(t, tc.status, target.Status)
			assert.Equal(t, tc.pending, target.PendingCleanup)
			for _, format := range [][]string{{}, {"-o", "wide"}} {
				out, err = run(append([]string{pruneCommand}, format...)...)
				require.NoError(t, err)
				assert.Contains(t, out, "Workspace/audit/dev")
				assert.Contains(t, out, tc.status)
			}
			out, err = run(pruneCommand, "-o", "yaml")
			require.NoError(t, err)
			if tc.pending != nil {
				assert.Contains(t, out, fmt.Sprintf("pendingCleanup: %d", *tc.pending))
			} else {
				assert.NotContains(t, out, "pendingCleanup")
			}
		})
	}
}

func TestPruneRejectsRemovedDeletionFlags(t *testing.T) {
	run := auditFixture(t)
	for _, flag := range []string{"--yes", "--dry-run", "--history-for=1h", "--from-plan=plan.json"} {
		_, err := run(pruneCommand, flag)
		require.ErrorContains(t, err, "unknown flag", flag)
	}
}

func decode(t *testing.T, value string) any {
	t.Helper()
	var result any
	require.NoError(t, json.Unmarshal([]byte(value), &result))
	return result
}

func editWorkspace(t *testing.T, responses map[string]json.RawMessage, edit func(map[string]any)) {
	t.Helper()
	path := "/apis/workspaces.rc.ayaka.io/v1alpha1/namespaces/audit/workspaces"
	var list map[string]any
	require.NoError(t, json.Unmarshal(responses[path], &list))
	edit(list["items"].([]any)[0].(map[string]any))
	data, err := json.Marshal(list)
	require.NoError(t, err)
	responses[path] = data
}

func TestDoctorRendersMissingRuntimeAndStorage(t *testing.T) {
	run := configuredAuditFixture(t, auditFixtureOptions{edit: func(responses map[string]json.RawMessage) {
		for _, path := range []string{"/api/v1/namespaces/audit/pods", "/api/v1/namespaces/audit/persistentvolumeclaims"} {
			var list map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(responses[path], &list))
			list["items"] = json.RawMessage(`[]`)
			data, err := json.Marshal(list)
			require.NoError(t, err)
			responses[path] = data
		}
	}})
	for _, args := range [][]string{{doctorCommand}, {doctorCommand, "-o", "json"}} {
		out, err := run(args...)
		require.NoError(t, err)
		assert.Contains(t, out, "MissingRuntime")
		assert.Contains(t, out, "MissingStorage")
	}
}
