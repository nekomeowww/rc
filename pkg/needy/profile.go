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

package needy

import (
	"fmt"
	"maps"
	"slices"

	"github.com/nekomeowww/rc/pkg/needy/profile"
)

// BuiltinProfile is the profile text that extends a profile shipped with
// needy.
func BuiltinProfile(name string) string {
	return "extends: builtin:" + name + "\n"
}

// ComposeProfile places a profile: it sets the actor's runner and the runner
// of every executor, and adds a native executor for a binding that the
// profile does not name. needy validates the rest when the run is created.
//
// A part without a runner would run on the daemon's own Pod, next to its
// state, so the profile may not choose runners itself, and every executor
// that it names must be bound. actor.network.services is refused too: its
// URLs bypass the gateway's egress policy and reach whatever the daemon can.
// actorEnv is set over the profile's actor.env.
func ComposeProfile(text, actorRunner string, executorRunners, actorEnv map[string]string) (string, error) {
	decoded, err := profile.Decode([]byte(text))
	if err != nil {
		return "", fmt.Errorf("profile is not valid: %w", err)
	}
	if decoded.Actor == nil {
		decoded.Actor = &profile.Actor{}
	}
	if decoded.Actor.Runner != nil {
		return "", fmt.Errorf("profile sets actor.runner; rc chooses the actor runner")
	}
	if decoded.Actor.Network != nil && len(decoded.Actor.Network.Services) > 0 {
		return "", fmt.Errorf("profile sets actor.network.services, which bypasses the gateway's egress policy")
	}
	decoded.Actor.Runner = &profile.RunnerSelector{Name: actorRunner}
	if len(actorEnv) > 0 && decoded.Actor.Env == nil {
		decoded.Actor.Env = make(map[string]string, len(actorEnv))
	}
	maps.Copy(decoded.Actor.Env, actorEnv)
	for _, name := range slices.Sorted(maps.Keys(decoded.Executors)) {
		if decoded.Executors[name].Runner != nil {
			return "", fmt.Errorf("profile executor %s sets runner; rc chooses executor runners", name)
		}
		if _, bound := executorRunners[name]; !bound {
			return "", fmt.Errorf("profile executor %s is not bound to a Workspace", name)
		}
	}
	if decoded.Executors == nil && len(executorRunners) > 0 {
		decoded.Executors = make(map[string]profile.Executor, len(executorRunners))
	}
	for name, runner := range executorRunners {
		executor, named := decoded.Executors[name]
		if !named {
			native := profile.ExecutorDriverNative
			executor.Driver = &native
		}
		executor.Runner = &profile.RunnerSelector{Name: runner}
		decoded.Executors[name] = executor
	}
	data, err := decoded.Encode()
	if err != nil {
		return "", fmt.Errorf("render profile: %w", err)
	}

	return string(data), nil
}
