package rckube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareTranscriptScope prevents Environment PVC clones from carrying another
// target's transcripts indefinitely. Call before starting the supervisor.
// A matching scope preserves history on restart. An absent legacy marker adopts
// the existing history without deleting it, for explicit upgrade compatibility.
// Ownership tombstones are always retained, even when copied from another scope.
func PrepareTranscriptScope(stateDirectory, scope string) error {
	if scope == "" {
		return nil
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	const marker = ".scope"
	previous, err := root.ReadFile(marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if string(previous) == scope {
		return nil
	}
	if len(previous) > 0 {
		directory, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if err := root.Remove(filepath.Join(entry.Name(), "transcript.log")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove inherited transcript: %w", err)
			}
		}
	}
	// Publish only after pruning succeeds. A crash before this rename repeats
	// idempotent cleanup before any new process can write its own transcript.
	if err := root.WriteFile(".scope.tmp", []byte(scope), 0o600); err != nil {
		return err
	}
	return root.Rename(".scope.tmp", marker)
}
