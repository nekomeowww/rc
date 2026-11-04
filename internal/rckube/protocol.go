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
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

// NOTICE: Adding an action or an optional field keeps the version. An older
// supervisor answers a new action with "unknown action", which callers treat
// as "no signal"; bumping the version would instead break every client during
// a rolling upgrade.
const protocolVersion = 4

type protocolRequest struct {
	Version  int                           `json:"version"`
	Action   string                        `json:"action"`
	UID      string                        `json:"uid,omitempty"`
	ID       string                        `json:"id,omitempty"`
	ClientID string                        `json:"clientID,omitempty"`
	Start    *processruntime.StartRequest  `json:"start,omitempty"`
	Rows     uint16                        `json:"rows,omitempty"`
	Columns  uint16                        `json:"columns,omitempty"`
	History  *processruntime.HistoryPolicy `json:"history,omitempty"`
	Lossless bool                          `json:"lossless,omitempty"`
	// CloseInputOnEOF sends input as frames whose end closes the process input.
	CloseInputOnEOF bool `json:"closeInputOnEOF,omitempty"`
}

type protocolResponse struct {
	Version int                           `json:"version"`
	State   processruntime.State          `json:"state,omitempty"`
	Entries []processruntime.HistoryEntry `json:"entries,omitempty"`
	// LastActivity answers the activity action. Nil means no activity was
	// recorded since the supervisor started.
	LastActivity *time.Time `json:"lastActivity,omitempty"`
	Error        string     `json:"error,omitempty"`
	Code         string     `json:"code,omitempty"`
}
