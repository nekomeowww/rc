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

package execution

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ServiceEnvironmentPrefix names the runtime container variables that carry
// service values read from a Secret or ConfigMap. Kubernetes resolves them when
// the container starts, so no secret value appears in the Pod spec. rc-kube
// removes them from its own environment before it starts any process.
const ServiceEnvironmentPrefix = "RC_SERVICE_ENV_"

// Service is the wire form of one resident process that rc-kube serve keeps
// running. It never contains a value read from a Secret.
type Service struct {
	Name             string   `json:"name"`
	Command          []string `json:"command"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	// Environment holds literal values.
	Environment map[string]string `json:"environment,omitempty"`
	// EnvironmentFrom maps a service variable to the runtime container
	// variable that holds its value.
	EnvironmentFrom map[string]string `json:"environmentFrom,omitempty"`
}

// EncodeServices serializes services for one rc-kube serve argument.
func EncodeServices(services []Service) (string, error) {
	data, err := json.Marshal(services)
	if err != nil {
		return "", fmt.Errorf("marshal services: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeServices reverses EncodeServices and validates each entry.
func DecodeServices(encoded string) ([]Service, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	var services []Service
	if err := json.Unmarshal(data, &services); err != nil {
		return nil, fmt.Errorf("unmarshal services: %w", err)
	}
	names := make(map[string]struct{}, len(services))
	for _, service := range services {
		if service.Name == "" {
			return nil, errors.New("service name is required")
		}
		if _, exists := names[service.Name]; exists {
			return nil, fmt.Errorf("service %s is defined twice", service.Name)
		}
		names[service.Name] = struct{}{}
		if len(service.Command) == 0 || service.Command[0] == "" {
			return nil, fmt.Errorf("service %s has no command", service.Name)
		}
	}

	return services, nil
}
