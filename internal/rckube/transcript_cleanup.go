package rckube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

// RemoveTranscript removes only the transcript belonging to (id, uid).
// Use on an offline volume; live supervisors must use PruneTranscript so a
// running writer cannot be unlinked. The tiny ownership tombstone remains:
// forgetting it would permit a delayed Start to execute the same UID again.
// os.Root confines filesystem operations even when a process directory is a symlink.
func RemoveTranscript(stateDirectory, id, uid string) error {
	if id == "" || uid == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return errors.New("process ID must be one safe path segment and UID is required")
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(id, 0o700); err != nil {
		return err
	}
	ownerPath := filepath.Join(id, "owner.uid")
	owner, err := root.ReadFile(ownerPath)
	if errors.Is(err, os.ErrNotExist) {
		// Fence a delayed start even if the original start never wrote its owner.
		file, createErr := root.OpenFile(ownerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return createErr
		}
		_, writeErr := file.WriteString(uid)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if string(owner) != uid {
		return ErrProcessConflict
	}
	if err := root.Remove(filepath.Join(id, "transcript.log")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove process transcript: %w", err)
	}
	return nil
}

// PruneTranscript drops a terminal transcript and its in-memory process record.
// Repeated requests, including after supervisor restart, are safe. Active
// processes and identity mismatches fail closed without changing their files.
func (supervisor *Supervisor) PruneTranscript(id, uid string) error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	process := supervisor.processes[id]
	if process != nil {
		process.mu.Lock()
		defer process.mu.Unlock()
		if process.state.UID != uid {
			return ErrProcessConflict
		}
		if process.state.Phase == phaseRunning || process.state.Phase == phaseStarting {
			return errors.New("cannot prune an active process")
		}
	}
	if err := RemoveTranscript(supervisor.stateDir, id, uid); err != nil {
		return err
	}
	delete(supervisor.processes, id)
	return nil
}

// RemoveTranscripts applies an offline batch independently to each identity.
// Sibling errors do not prevent progress; replay is safe after partial success.
func RemoveTranscripts(stateDirectory string, batch []processruntime.TranscriptIdentity) error {
	var failures []error
	for _, entry := range batch {
		if err := RemoveTranscript(stateDirectory, entry.ID, entry.UID); err != nil {
			failures = append(failures, fmt.Errorf("prune %s: %w", entry.ID, err))
		}
	}
	return errors.Join(failures...)
}
