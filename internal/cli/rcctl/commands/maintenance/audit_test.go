package maintenance

import (
	"bytes"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nekomeowww/rc/internal/audit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dryRunArg = "--dry-run"
const fromPlanArg = "--from-plan"

const yesArg = "--yes"
const planPlaceholder = "PLAN"

type auditFixtureOptions struct {
	input              string
	missingEvaluator   bool
	allowHistoryDelete bool
	edit               func(map[string]json.RawMessage)
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
		if serveFixtureObject(t, w, r, responses, options.allowHistoryDelete) {
			return
		}
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
		// Explicitly injected test approval keeps fixture policy independent from
		// the production canonical evaluator.
		var evaluate audit.HistoryEvaluator = func(records []workspaces.WorkspaceExec, _ client.Object, _ time.Time) (executionretention.Plan, error) {
			plan := executionretention.Plan{}
			for i := range records {
				if records[i].Spec.Retain {
					plan.Keep = append(plan.Keep, i)
				} else {
					plan.Remove = append(plan.Remove, i)
				}
			}
			return plan, nil
		}
		if options.missingEvaluator {
			evaluate = nil
		}
		cmd.AddCommand(newDoctorCommand(flags), newPruneCommand(flags, evaluate))
		out := new(bytes.Buffer)
		cmd.SetIn(strings.NewReader(options.input))
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append([]string{"--kubeconfig", config}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
}

// serveFixtureObject supports fresh record/target reads. Only the designated
// history object can be deleted; the read-only cases reject every write.
func serveFixtureObject(t *testing.T, w http.ResponseWriter, r *http.Request, responses map[string]json.RawMessage, allowDelete bool) bool {
	t.Helper()
	index := strings.LastIndex(r.URL.Path, "/")
	body, ok := responses[r.URL.Path[:index]]
	if !ok {
		return false
	}
	var list struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Items      []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || !strings.HasSuffix(list.Kind, "List") {
		return false
	}
	for i, item := range list.Items {
		var object struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(item, &object))
		if object.Metadata.Name != r.URL.Path[index+1:] {
			continue
		}
		if r.Method == http.MethodGet {
			_, err := w.Write(item)
			assert.NoError(t, err)
			return true
		}
		if !allowDelete || r.Method != http.MethodDelete || object.Metadata.Name != "old-exec" {
			return false
		}
		var opts metav1.DeleteOptions
		require.NoError(t, json.NewDecoder(r.Body).Decode(&opts))
		require.NotNil(t, opts.Preconditions)
		assert.Equal(t, object.Metadata.UID, *opts.Preconditions.UID)
		assert.Equal(t, object.Metadata.ResourceVersion, *opts.Preconditions.ResourceVersion)
		assert.Equal(t, metav1.DeletePropagationOrphan, *opts.PropagationPolicy)
		list.Items = append(list.Items[:i], list.Items[i+1:]...)
		encoded, err := json.Marshal(list)
		require.NoError(t, err)
		responses[r.URL.Path[:index]] = encoded
		assert.NoError(t, json.NewEncoder(w).Encode(metav1.Status{Status: "Success"}))
		return true
	}
	w.WriteHeader(http.StatusNotFound)
	assert.NoError(t, json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}))
	return true
}

func TestDoctorReportsEvidenceAndPlanContainsOnlyHistory(t *testing.T) {
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
	out, err = run("prune", dryRunArg, "-o", "json")
	require.NoError(t, err)
	var plan audit.CleanupPlan
	require.NoError(t, json.Unmarshal([]byte(out), &plan))
	require.Len(t, plan.Candidates, 1)
	assert.Equal(t, "old-exec", plan.Candidates[0].Name)
	assert.Less(t, len(out), 900, "plan does not serialize diagnostic inventory or impact")
}

func TestPruneSavedPlanAndPreviewModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		expired   bool
		wantError string
	}{
		{"fresh-dry-run", []string{dryRunArg, yesArg}, false, ""},
		{"saved-dry-run", []string{fromPlanArg, planPlaceholder, dryRunArg, yesArg}, false, ""},
		{"declined", nil, false, "cancelled"},
		{"expired-execution", []string{fromPlanArg, planPlaceholder, yesArg}, true, "expired"},
		{"expired-preview", []string{fromPlanArg, planPlaceholder, dryRunArg}, true, "expired"},
		{"policy-override", []string{fromPlanArg, planPlaceholder, "--history-for", "1h"}, false, "cannot override"},
		{"scope-mismatch", []string{fromPlanArg, planPlaceholder, "--namespace", "other"}, false, "scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := auditFixture(t)
			out, err := run("prune", dryRunArg, "-o", "json")
			require.NoError(t, err)
			var plan audit.CleanupPlan
			require.NoError(t, json.Unmarshal([]byte(out), &plan))
			if tc.expired {
				plan.ExpiresAt = metav1.NewTime(time.Now().Add(-time.Hour))
			}
			data, err := json.Marshal(plan)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "plan.json")
			require.NoError(t, os.WriteFile(path, data, 0600))
			args := append([]string{"prune", "-o", "json"}, tc.args...)
			for i, arg := range args {
				if arg == planPlaceholder {
					args[i] = path
				}
			}
			preview, err := run(args...)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, out, preview)
		})
	}
}

func TestPruneSavedPlanConfirmationAndRetry(t *testing.T) {
	run := configuredAuditFixture(t, auditFixtureOptions{input: "yes\n", allowHistoryDelete: true})
	out, err := run("prune", dryRunArg, "-o", "json")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, os.WriteFile(path, []byte(out), 0600))
	out, err = run("prune", fromPlanArg, path, "-o", "json")
	require.NoError(t, err)
	var result audit.PruneResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Len(t, result.Requested, 1)
	require.Len(t, result.Absent, 1)
	out, err = run("prune", fromPlanArg, path, yesArg, "-o", "json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Empty(t, result.Requested)
	assert.Len(t, result.Absent, 1)
}

func TestPruneUnavailableEvaluatorCannotAuthorizeDeletion(t *testing.T) {
	run := configuredAuditFixture(t, auditFixtureOptions{missingEvaluator: true})
	out, err := run("prune", dryRunArg, "-o", "json")
	require.NoError(t, err)
	var plan audit.CleanupPlan
	require.NoError(t, json.Unmarshal([]byte(out), &plan))
	assert.Empty(t, plan.Candidates)
	_, err = run("prune", yesArg)
	require.ErrorContains(t, err, "canonical history evaluator unavailable")
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
