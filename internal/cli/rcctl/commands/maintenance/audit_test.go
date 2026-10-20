package maintenance

import (
	"bytes"
	"encoding/json"
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
		cmd.AddCommand(newDoctorCommand(flags))
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
