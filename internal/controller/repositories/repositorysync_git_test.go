package repositories

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestSyncAdvancesParentAndPreservesDirtyChild(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Repository Jobs run the POSIX script on Linux")
	}
	directory := t.TempDir()
	remote := filepath.Join(directory, "remote")
	parent := filepath.Join(directory, "parent")
	child := filepath.Join(directory, "child")
	require.NoError(t, os.MkdirAll(remote, 0755))
	require.NoError(t, os.MkdirAll(parent, 0755))
	git := func(path string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = path
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(directory, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return strings.TrimSpace(string(output))
	}
	git(remote, "init", "-b", "main")
	git(remote, "config", "user.email", "sync@example.test")
	git(remote, "config", "user.name", "Sync test")
	file := filepath.Join(remote, "tracked.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0644))
	git(remote, "add", ".")
	git(remote, "commit", "-m", "first")
	first := git(remote, "rev-parse", "HEAD")
	synchronize := func() {
		t.Helper()
		// Substitute the mount and termination-log paths; execute the production fetch,
		// ref resolution, reset, clean, and submodule policy against real Git.
		script := strings.NewReplacer("/repository", parent, "/dev/termination-log", filepath.Join(directory, "commit")).Replace(repositoryCheckoutScript)
		command := exec.CommandContext(t.Context(), "sh", "-ceu", script, "repository-sync", remote, "refs/heads/main", repositorySubmoduleModeNone, "Fail", "none", "0")
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(directory, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		commit, err := os.ReadFile(filepath.Join(directory, "commit"))
		require.NoError(t, err)
		require.Equal(t, git(parent, "rev-parse", "HEAD"), strings.TrimSpace(string(commit)))
	}
	synchronize()
	git(directory, "clone", parent, child)
	require.NoError(t, os.WriteFile(filepath.Join(child, "tracked.txt"), []byte("local edit"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(child, "untracked.txt"), []byte("keep"), 0644))
	require.NoError(t, os.WriteFile(file, []byte("second"), 0644))
	git(remote, "commit", "-am", "second")
	second := git(remote, "rev-parse", "HEAD")
	synchronize()
	require.Equal(t, second, git(parent, "rev-parse", "HEAD"))
	require.Equal(t, first, git(child, "rev-parse", "HEAD"))
	contents, err := os.ReadFile(filepath.Join(child, "tracked.txt"))
	require.NoError(t, err)
	require.Equal(t, "local edit", string(contents))
	contents, err = os.ReadFile(filepath.Join(child, "untracked.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(contents))
	fresh := filepath.Join(directory, "fresh")
	git(directory, "clone", parent, fresh)
	require.Equal(t, second, git(fresh, "rev-parse", "HEAD"))
}

func TestCheckoutReportsSubmoduleMissingUpstream(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Repository Jobs run the POSIX script on Linux")
	}
	for _, policy := range []repositoriesv1alpha1.RepositorySubmoduleFailurePolicy{
		repositoriesv1alpha1.RepositorySubmoduleFailurePolicyFail, repositoriesv1alpha1.RepositorySubmoduleFailurePolicyContinue,
	} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			checkoutWithMissingSubmodule(t, policy)
		})
	}
}

func checkoutWithMissingSubmodule(t *testing.T, policy repositoriesv1alpha1.RepositorySubmoduleFailurePolicy) {
	t.Helper()
	directory := t.TempDir()
	gitConfig := filepath.Join(directory, "gitconfig")
	environment := append(os.Environ(), "GIT_CONFIG_GLOBAL="+gitConfig, "GIT_CONFIG_NOSYSTEM=1")
	git := func(path string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = path
		command.Env = environment
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return strings.TrimSpace(string(output))
	}
	repository := func(name string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		require.NoError(t, os.MkdirAll(path, 0755))
		git(path, "init", "-b", "main")
		require.NoError(t, os.WriteFile(filepath.Join(path, "README"), []byte(name), 0644))
		git(path, "add", ".")
		git(path, "commit", "-m", name)
		return path
	}
	git(directory, "config", "--file", gitConfig, "user.email", "sync@example.test")
	git(directory, "config", "--file", gitConfig, "user.name", "Sync test")
	git(directory, "config", "--file", gitConfig, "protocol.file.allow", "always")
	healthy := repository("healthy")
	broken := repository("broken")
	remote := repository("remote")
	git(remote, "submodule", "add", healthy, "healthy")
	git(remote, "submodule", "add", broken, "broken")
	git(remote, "commit", "-m", "add submodules")
	// Rewrite the submodule upstream so the parent's recorded commit is gone,
	// as when an upstream force-pushes over a commit a parent still records.
	recorded := git(broken, "rev-parse", "HEAD")
	git(broken, "checkout", "--orphan", "rewritten")
	git(broken, "commit", "-m", "rewritten")
	git(broken, "branch", "-D", "main")
	git(broken, "reflog", "expire", "--expire=now", "--all")
	git(broken, "gc", "--prune=now")

	parent := filepath.Join(directory, "parent")
	require.NoError(t, os.MkdirAll(parent, 0755))
	result := filepath.Join(directory, "result")
	script := strings.NewReplacer("/repository", parent, "/dev/termination-log", result).Replace(repositoryCheckoutScript)
	command := exec.CommandContext(t.Context(), "sh", "-ceu", script, "repository-sync", remote, "refs/heads/main", repositorySubmoduleModeDirect, string(policy), "none", "0")
	command.Env = environment
	output, err := command.CombinedOutput()
	if policy == repositoriesv1alpha1.RepositorySubmoduleFailurePolicyContinue {
		require.NoError(t, err, "Continue keeps the parent checkout: %s", output)
	} else {
		require.Error(t, err, "Fail must not publish incomplete source state")
		require.Contains(t, string(output), "failurePolicy to Continue")
	}

	message, err := os.ReadFile(result)
	require.NoError(t, err)
	parsed := parseCheckoutResult(string(message))
	require.Equal(t, git(remote, "rev-parse", "HEAD"), parsed.commit)
	require.Equal(t, []string{"broken at " + recorded}, parsed.submoduleFailures, "name every failed path in either mode")
	require.Equal(t, git(healthy, "rev-parse", "HEAD"), git(filepath.Join(parent, "healthy"), "rev-parse", "HEAD"), "update the healthy submodule")
	require.NoFileExists(t, filepath.Join(parent, ".git", "rc-submodule-failures"))
}
