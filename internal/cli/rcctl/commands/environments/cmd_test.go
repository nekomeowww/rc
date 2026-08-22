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

package environments

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const testNoPassthroughFlag = "no-env-passthrough"

func TestEnvironmentExecParsesOptionsAfterEnvironmentName(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	command := newExecCommand(new(kubeconfig.Flags))

	requirements.NoError(command.ParseFlags([]string{
		"prepared", "--" + testNoPassthroughFlag, "--", "sh", "--" + testNoPassthroughFlag,
	}), "parse rcctl options until the optional command separator")
	noPassthrough, err := command.Flags().GetBool(testNoPassthroughFlag)
	requirements.NoError(err, "read parsed environment pass-through option")
	assertions.True(noPassthrough, "accept rcctl options after the Environment name")
	assertions.Equal(
		[]string{"sh", "--" + testNoPassthroughFlag},
		commandAfterEnvironmentName(command.Flags().Args()),
		"preserve command arguments after the separator",
	)
}
