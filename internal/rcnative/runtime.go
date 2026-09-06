// Package rcnative describes the filesystem and local endpoint of the current
// rc-kube binary. It deliberately has no Kubernetes dependencies.
package rcnative

import "path/filepath"

// Runtime is the native rc-kube layout selected at compile time.
type Runtime struct {
	Home      string
	Workspace string
	Run       string
	Endpoint  string
}

// Current returns the layout for the operating system running this binary.
func Current() Runtime { return current() }

// StateDirectory contains durable process state and transcripts.
func (runtime Runtime) StateDirectory() string {
	return filepath.Join(runtime.Home, ".rc", "processes")
}

// ScriptCommand constructs the native shell command for one lifecycle script.
func (runtime Runtime) ScriptCommand(script string) []string { return scriptCommand(script) }
