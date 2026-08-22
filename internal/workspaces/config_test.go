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

package workspaces

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultStoreScopesValuesByContextAndNamespace(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := DefaultStore{Path: path}

	requirements.NoError(store.Set("kind-dev", "team-a", Defaults{Workspace: "coding", Environment: "node-rust"}), "persist defaults")
	persisted, err := store.Get("kind-dev", "team-a")
	requirements.NoError(err, "reload defaults")
	assertions.Equal(Defaults{Workspace: "coding", Environment: "node-rust"}, persisted)

	otherNamespace, err := store.Get("kind-dev", "team-b")
	requirements.NoError(err, "read another namespace")
	assertions.Empty(otherNamespace, "defaults do not leak across namespaces")
}
