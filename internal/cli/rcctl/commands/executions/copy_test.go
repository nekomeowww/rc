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

package executions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

func TestParseCopyOperand(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]copyOperand{
		"dev:/home/agent/report.html": {workspace: "dev", path: "/home/agent/report.html"},
		"my-ws:artifacts/":            {workspace: "my-ws", path: "artifacts/"},
		"dev:":                        {workspace: "dev", path: ""},
		"./local/file":                {path: "./local/file"},
		`C:\Users\person\file`:        {path: `C:\Users\person\file`},
		"Upper:case":                  {path: "Upper:case"},
	} {
		assert.Equal(t, want, parseCopyOperand(input), input)
	}
}

func TestCopyRequiresExactlyOneWorkspaceOperand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"a", "b"}, {"dev:a", "dev:b"}} {
		command := newCopyCommand(kubeconfig.NewFlags())
		command.SetArgs(args)
		command.SilenceUsage = true
		err := command.Execute()
		require.ErrorContains(t, err, "exactly one of SOURCE and DESTINATION")
	}
}
