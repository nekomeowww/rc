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

// Package needy is rc's client for needy: the daemon's control-plane API,
// runner credentials and static client tokens, the daemon's static files,
// the command lines of the daemon and its runners, and profile composition.
// It knows needy's formats and nothing of how rc names or places needy on
// Kubernetes.
package needy

// ImageExecutable is needy in the needy image.
const ImageExecutable = "/usr/local/bin/needy"

// Role is a runner role, as the CLI and the runners file spell it.
type Role string

// needy's runner roles.
const (
	RoleActor    Role = "actor"
	RoleExecutor Role = "executor"
)
