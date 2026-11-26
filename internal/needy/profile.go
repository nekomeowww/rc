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

	"sigs.k8s.io/yaml"
)

// Profile keys and values that rc sets.
const (
	profileDriver       = "driver"
	profileNativeDriver = "native"
	profileRunner       = "runner"
)

// BuiltinProfile is the profile text that extends a profile shipped with
// needy.
func BuiltinProfile(name string) string {
	return "extends: builtin:" + name + "\n"
}

// ComposeProfile places a profile: it sets the actor's runner and the runner
// of every executor, and adds a native executor for a binding that the
// profile does not name. needy validates the rest.
//
// A part without a runner would run on the daemon's own Pod, next to its
// state, so the profile may not choose runners itself, and every executor
// that it names must be bound. actor.network.services is refused too: its
// URLs bypass the gateway's egress policy and reach whatever the daemon can.
func ComposeProfile(text, actorRunner string, executorRunners map[string]string) (string, error) {
	var profile map[string]any
	if err := yaml.Unmarshal([]byte(text), &profile); err != nil {
		return "", fmt.Errorf("profile is not valid YAML: %w", err)
	}
	if profile == nil {
		profile = make(map[string]any)
	}
	actor, err := mapping(profile, "actor")
	if err != nil {
		return "", err
	}
	if _, set := actor[profileRunner]; set {
		return "", fmt.Errorf("profile sets actor.runner; rc chooses the actor runner")
	}
	if network, set := actor["network"].(map[string]any); set {
		if _, set := network["services"]; set {
			return "", fmt.Errorf("profile sets actor.network.services, which bypasses the gateway's egress policy")
		}
	}
	actor[profileRunner] = actorRunner
	executors, err := mapping(profile, "executors")
	if err != nil {
		return "", err
	}
	for _, name := range slices.Sorted(maps.Keys(executors)) {
		executor, ok := executors[name].(map[string]any)
		if !ok {
			return "", fmt.Errorf("profile executor %s must be a mapping", name)
		}
		if _, set := executor[profileRunner]; set {
			return "", fmt.Errorf("profile executor %s sets runner; rc chooses executor runners", name)
		}
		if _, bound := executorRunners[name]; !bound {
			return "", fmt.Errorf("profile executor %s is not bound to a Workspace", name)
		}
	}
	for name, runner := range executorRunners {
		executor, ok := executors[name].(map[string]any)
		if !ok {
			executor = map[string]any{profileDriver: profileNativeDriver}
			executors[name] = executor
		}
		executor[profileRunner] = runner
	}
	if len(executors) == 0 {
		delete(profile, "executors")
	}
	data, err := yaml.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("render profile: %w", err)
	}

	return string(data), nil
}

// mapping returns profile[key] as a mapping, creating it when it is absent.
func mapping(profile map[string]any, key string) (map[string]any, error) {
	value, set := profile[key]
	if !set || value == nil {
		created := make(map[string]any)
		profile[key] = created

		return created, nil
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("profile %s must be a mapping", key)
	}

	return result, nil
}
