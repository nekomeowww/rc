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

package v1alpha1

// Labels that rc puts on the objects it creates for needy.
const (
	// NeedyComponentLabel marks an object that rc creates for needy; its value
	// names the part, such as daemon or executor.
	NeedyComponentLabel = "rc.ayaka.io/needy"
	// NeedyWorkspaceLabel names the Workspace that a needy executor runner
	// serves.
	NeedyWorkspaceLabel = "rc.ayaka.io/workspace"
	// NeedyActorLabel names the Actor of an actor runner Pod.
	NeedyActorLabel = "rc.ayaka.io/actor"
)
