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
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const (
	recordFileName = "record.json"
	// phaseLost marks a record whose supervisor stopped before the process
	// finished. The process cannot be observed again.
	phaseLost                 = "Lost"
	reasonSupervisorRestarted = "SupervisorRestarted"
)

// historyTerminal reports a phase that no supervisor will change again.
func historyTerminal(phase string) bool {
	return phase == phaseExited || phase == phaseStopped || phase == phaseLost
}

// recordLocked replaces the process record with its current state. The caller
// holds process.mu. History is diagnostic: a failed write never changes the
// process outcome, so the error is returned only for tests and logging.
func (process *supervisedProcess) recordLocked() error {
	entry := processruntime.HistoryEntry{
		State:            cloneState(process.state),
		Command:          process.command,
		WorkingDirectory: process.workingDirectory,
		TTY:              process.tty,
		StartedAt:        process.startedAt,
		CompletedAt:      process.completedAt,
	}
	entry.AttachedClients = 0

	return writeRecord(process.supervisor.stateDir, entry)
}

func writeRecord(stateDirectory string, entry processruntime.HistoryEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode process record: %w", err)
	}
	root, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	temporary := filepath.Join(entry.ID, recordFileName+".tmp")
	if err := root.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write process record: %w", err)
	}

	return root.Rename(temporary, filepath.Join(entry.ID, recordFileName))
}

func readRecord(root *os.Root, id string) (processruntime.HistoryEntry, bool, error) {
	data, err := root.ReadFile(filepath.Join(id, recordFileName))
	if errors.Is(err, os.ErrNotExist) {
		return processruntime.HistoryEntry{}, false, nil
	}
	if err != nil {
		return processruntime.HistoryEntry{}, false, err
	}
	entry := processruntime.HistoryEntry{}
	if err := json.Unmarshal(data, &entry); err != nil {
		return processruntime.HistoryEntry{}, false, fmt.Errorf("decode process record %s: %w", id, err)
	}

	return entry, true, nil
}

// readRecords returns every process record in the state directory. Legacy
// directories without a record are skipped.
func readRecords(stateDirectory string) ([]processruntime.HistoryEntry, error) {
	root, err := os.OpenRoot(stateDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	records := make([]processruntime.HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, found, err := readRecord(root, entry.Name())
		if err != nil {
			return nil, err
		}
		if found {
			records = append(records, record)
		}
	}

	return records, nil
}

// RecoverHistory marks records left Starting or Running by a previous
// supervisor as Lost. Run it before the supervisor accepts requests.
func (supervisor *Supervisor) RecoverHistory(now time.Time) error {
	records, err := readRecords(supervisor.stateDir)
	if err != nil {
		return err
	}
	for _, record := range records {
		if historyTerminal(record.Phase) {
			continue
		}
		record.Phase = phaseLost
		record.Reason = reasonSupervisorRestarted
		record.PID = 0
		completed := now.UTC()
		record.CompletedAt = &completed
		if err := writeRecord(supervisor.stateDir, record); err != nil {
			return err
		}
	}

	return nil
}

// inspectRecord returns a finished process that a previous supervisor
// recorded. Lost and unfinished records stay ErrNotFound, so the controller
// still treats them as lost.
func (supervisor *Supervisor) inspectRecord(id string) (processruntime.State, error) {
	if filepath.Base(id) != id || id == "." || id == ".." {
		return processruntime.State{}, processruntime.ErrNotFound
	}
	root, err := os.OpenRoot(supervisor.stateDir)
	if err != nil {
		return processruntime.State{}, processruntime.ErrNotFound
	}
	defer func() { _ = root.Close() }()
	record, found, err := readRecord(root, id)
	if err != nil {
		return processruntime.State{}, err
	}
	if !found || (record.Phase != phaseExited && record.Phase != phaseStopped) {
		return processruntime.State{}, processruntime.ErrNotFound
	}

	return record.State, nil
}

// List returns every recorded process, newest start first. Processes this
// supervisor owns report their live state.
func (supervisor *Supervisor) List() ([]processruntime.HistoryEntry, error) {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	records, err := readRecords(supervisor.stateDir)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]int, len(records))
	for index, record := range records {
		byID[record.ID] = index
	}
	for id, process := range supervisor.processes {
		process.mu.Lock()
		live := processruntime.HistoryEntry{
			State:            cloneState(process.state),
			Command:          process.command,
			WorkingDirectory: process.workingDirectory,
			TTY:              process.tty,
			StartedAt:        process.startedAt,
			CompletedAt:      process.completedAt,
		}
		process.mu.Unlock()
		if index, ok := byID[id]; ok {
			records[index] = live
		} else {
			records = append(records, live)
		}
	}
	slices.SortFunc(records, func(left, right processruntime.HistoryEntry) int {
		if order := compareTimes(right.StartedAt, left.StartedAt); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})

	return records, nil
}

// PruneHistory removes finished processes beyond the policy. It returns the
// number of removed records. Active processes and legacy directories without a
// record are never removed. Removing a process directory also removes its
// at-most-once identity, which is safe only because no start request outlives
// the policy age and a WorkspaceExec record is never started twice.
func (supervisor *Supervisor) PruneHistory(policy processruntime.HistoryPolicy, now time.Time) (int, error) {
	if policy.TTLAfterFinished <= 0 && policy.MaxEntries <= 0 && policy.TranscriptTTL <= 0 {
		return 0, nil
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	records, err := readRecords(supervisor.stateDir)
	if err != nil {
		return 0, err
	}
	finished := make([]processruntime.HistoryEntry, 0, len(records))
	for _, record := range records {
		if !historyTerminal(record.Phase) || record.CompletedAt == nil {
			continue
		}
		if process := supervisor.processes[record.ID]; process != nil && !process.finished() {
			continue
		}
		finished = append(finished, record)
	}
	slices.SortFunc(finished, func(left, right processruntime.HistoryEntry) int {
		return compareTimes(right.CompletedAt, left.CompletedAt)
	})
	root, err := os.OpenRoot(supervisor.stateDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	removed := 0
	for index, record := range finished {
		age := now.Sub(*record.CompletedAt)
		expired := policy.TTLAfterFinished > 0 && age > policy.TTLAfterFinished
		overflow := policy.MaxEntries > 0 && index >= policy.MaxEntries
		if expired || overflow {
			if err := root.RemoveAll(record.ID); err != nil {
				return removed, fmt.Errorf("remove process history %s: %w", record.ID, err)
			}
			delete(supervisor.processes, record.ID)
			removed++
			continue
		}
		if policy.TranscriptTTL > 0 && age > policy.TranscriptTTL {
			if err := root.Remove(filepath.Join(record.ID, "transcript.log")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, fmt.Errorf("remove process transcript %s: %w", record.ID, err)
			}
		}
	}

	return removed, nil
}

func (process *supervisedProcess) finished() bool {
	process.mu.Lock()
	defer process.mu.Unlock()

	return historyTerminal(process.state.Phase)
}

// compareTimes orders nil before every time. Callers that sort newest first
// swap the arguments, which also puts nil last.
func compareTimes(left, right *time.Time) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	default:
		return left.Compare(*right)
	}
}
