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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

type Defaults struct {
	Workspace   string `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Environment string `json:"environment,omitempty" yaml:"environment,omitempty"`
}

type contextDefaults struct {
	Namespaces map[string]Defaults `json:"namespaces,omitempty" yaml:"namespaces,omitempty"`
}

type defaultsFile struct {
	Contexts map[string]contextDefaults `json:"contexts,omitempty" yaml:"contexts,omitempty"`
}

type DefaultStore struct {
	Path string
}

func DefaultConfigPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve XDG configuration directory: %w", err)
	}

	return filepath.Join(directory, "rc", "config.yaml"), nil
}

func (store DefaultStore) Get(contextName string, namespace string) (Defaults, error) {
	config, err := store.load()
	if err != nil {
		return Defaults{}, err
	}
	contextConfig, ok := config.Contexts[contextName]
	if !ok {
		return Defaults{}, nil
	}

	return contextConfig.Namespaces[namespace], nil
}

func (store DefaultStore) Set(contextName string, namespace string, defaults Defaults) error {
	config, err := store.load()
	if err != nil {
		return err
	}
	if config.Contexts == nil {
		config.Contexts = make(map[string]contextDefaults)
	}
	contextConfig := config.Contexts[contextName]
	if contextConfig.Namespaces == nil {
		contextConfig.Namespaces = make(map[string]Defaults)
	}
	contextConfig.Namespaces[namespace] = defaults
	config.Contexts[contextName] = contextConfig

	return store.save(config)
}

func (store DefaultStore) load() (defaultsFile, error) {
	data, err := os.ReadFile(store.Path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultsFile{Contexts: make(map[string]contextDefaults)}, nil
	}
	if err != nil {
		return defaultsFile{}, fmt.Errorf("read rc XDG config: %w", err)
	}
	config := defaultsFile{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return defaultsFile{}, fmt.Errorf("decode rc XDG config: %w", err)
	}
	if config.Contexts == nil {
		config.Contexts = make(map[string]contextDefaults)
	}

	return config, nil
}

func (store DefaultStore) save(config defaultsFile) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode rc XDG config: %w", err)
	}
	directory := filepath.Dir(store.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create rc XDG config directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary rc XDG config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict temporary rc XDG config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary rc XDG config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary rc XDG config: %w", err)
	}
	if err := os.Rename(temporaryPath, store.Path); err != nil {
		return fmt.Errorf("replace rc XDG config: %w", err)
	}

	return nil
}
