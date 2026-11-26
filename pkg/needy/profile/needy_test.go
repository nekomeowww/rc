package profile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNeedyReadsReencodedProfilesTheSame runs the needy binary named by
// NEEDY_BIN on every fixture and on its re-encoded form, and compares what
// needy makes of each: the document (`config profile show`), the document
// merged along `extends` (`show --resolved`), and `config profile check`.
//
// The fixtures are copied into two trees, one as written and one
// re-encoded, at the same relative paths, so that `extends` and recipe
// references resolve alike; each tree's root is replaced in needy's output
// before comparing. Set NEEDY_KEEP=1 to keep the trees.
func TestNeedyReadsReencodedProfilesTheSame(t *testing.T) {
	bin := os.Getenv("NEEDY_BIN")
	if bin == "" {
		t.Skip("NEEDY_BIN is not set")
	}
	base := t.TempDir()
	if os.Getenv("NEEDY_KEEP") != "" {
		var err error
		base, err = os.MkdirTemp("", "needy-profile-")
		require.NoError(t, err)
		t.Logf("trees kept in %s", base)
	}
	original := filepath.Join(base, "original")
	reencoded := filepath.Join(base, "reencoded")
	// An empty configuration directory, so no user configuration is read.
	config := filepath.Join(base, "config", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(config), 0o755))

	names := fixtures(t)
	require.NoError(t, copyTree("testdata", original))
	require.NoError(t, copyTree("testdata", reencoded))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(t, err)
		p, err := Decode(data)
		require.NoError(t, err, name)
		encoded, err := p.Encode()
		require.NoError(t, err, name)
		require.NoError(t, os.WriteFile(filepath.Join(reencoded, name), encoded, 0o644))
	}

	run := func(root, name string, args ...string) string {
		path := filepath.Join(root, name)
		full := append([]string{"--config", config, "config", "profile"}, args...)
		full = append(full, path)
		cmd := exec.Command(bin, full...)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			require.NoError(t, err)
		}
		text := strings.ReplaceAll(out.String(), root, "<root>")
		return "exit " + strconv.Itoa(code) + "\n" + text
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{
				{"show", "-o", "json"},
				{"show", "--resolved", "-o", "json"},
				{"check"},
			} {
				want := run(original, name, args...)
				got := run(reencoded, name, args...)
				assert.Equal(t, want, got, "needy config profile %s", strings.Join(args, " "))
				t.Logf("%s: %s", strings.Join(args, " "), firstLine(want, 1))
			}
		})
	}
}

// firstLine is the line at index n of text, for a short log of each
// result.
func firstLine(text string, n int) string {
	lines := strings.Split(text, "\n")
	if n < len(lines) {
		return lines[0] + " " + lines[n]
	}
	return lines[0]
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
