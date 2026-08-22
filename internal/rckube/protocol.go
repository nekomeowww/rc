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

import processruntime "github.com/nekomeowww/rc/internal/agentprocess"

const protocolVersion = 2

type protocolRequest struct {
	Version  int                          `json:"version"`
	Action   string                       `json:"action"`
	ID       string                       `json:"id,omitempty"`
	ClientID string                       `json:"clientID,omitempty"`
	Start    *processruntime.StartRequest `json:"start,omitempty"`
	Rows     uint16                       `json:"rows,omitempty"`
	Columns  uint16                       `json:"columns,omitempty"`
}

type protocolResponse struct {
	Version int                  `json:"version"`
	State   processruntime.State `json:"state,omitempty"`
	Error   string               `json:"error,omitempty"`
	Code    string               `json:"code,omitempty"`
}
