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
}

// TestPruneReportsBacklogReadOnly relies on the fixture server failing every
// non-GET request: prune must never delete history itself.
func TestPruneReportsBacklogReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention string
		want      string
		pending   int
	}{
		{"unset", "", audit.RetentionUnset, 0},
		{"configured", `{"ttlAfterFinished":"24h"}`, audit.RetentionConfigured, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := configuredAuditFixture(t, auditFixtureOptions{edit: func(responses map[string]json.RawMessage) {
				if tc.retention != "" {
					setWorkspaceRetention(t, responses, tc.retention)
				}
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
			assert.Equal(t, tc.want, target.Retention)
			assert.Equal(t, tc.pending, target.PendingCleanup)
			assert.Equal(t, 1-tc.pending, target.Retained)
			for _, format := range [][]string{{}, {"-o", "wide"}} {
				out, err = run(append([]string{pruneCommand}, format...)...)
				require.NoError(t, err)
				assert.Contains(t, out, "Workspace/audit/dev")
				assert.Contains(t, out, tc.want)
			}
			out, err = run(pruneCommand, "-o", "yaml")
			require.NoError(t, err)
			assert.Contains(t, out, fmt.Sprintf("pendingCleanup: %d", tc.pending))
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

func setWorkspaceRetention(t *testing.T, responses map[string]json.RawMessage, retention string) {
	t.Helper()
	path := "/apis/workspaces.rc.ayaka.io/v1alpha1/namespaces/audit/workspaces"
	var list map[string]any
	require.NoError(t, json.Unmarshal(responses[path], &list))
	var policy any
	require.NoError(t, json.Unmarshal([]byte(retention), &policy))
	workspace := list["items"].([]any)[0].(map[string]any)
	workspace["spec"].(map[string]any)["executionRetention"] = policy
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
