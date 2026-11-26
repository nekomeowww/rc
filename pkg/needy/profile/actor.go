package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Actor is the machine where the driving program runs.
type Actor struct {
	// Driver is what runs the actor; required, needy never picks one.
	Driver *ActorDriver `json:"driver,omitzero"`
	// Image is an OCI image reference, an image file path, or an inline image
	// definition.
	Image *Image `json:"image,omitzero"`
	// Runner is the runner the actor runs on; the control host when absent.
	Runner *RunnerSelector `json:"runner,omitzero"`
	// Recipe is a recipe reference: `builtin:NAME` or a path relative to the
	// profile.
	Recipe *string `json:"recipe,omitzero"`
	// Seed is a stable machine identity seed, an integer or a string label.
	Seed *Seed `json:"seed,omitzero"`
	// Limits restricts the driving program's processes.
	Limits *Limits `json:"limits,omitzero"`
	// Network is the actor's network at its gateway.
	Network *ActorNetwork `json:"network,omitzero"`
	// Command is the driving program.
	Command *Words `json:"command,omitzero"`
	// Env is the driving program's environment.
	Env map[string]string `json:"env,omitzero"`
	// Workspace is the directory the work operates on; the home directory
	// when absent.
	Workspace *Workspace `json:"workspace,omitzero"`
	// Container holds a container driver's options.
	Container *ContainerActor `json:"container,omitzero"`
}

func (a *Actor) checkComplete() error {
	if a.Driver == nil {
		return errors.New("missing field `driver`")
	}
	if a.Workspace != nil && a.Workspace.Path == nil {
		return errors.New("workspace: missing field `path`")
	}
	if a.Network != nil {
		for _, name := range sortedKeys(a.Network.Services) {
			if a.Network.Services[name].URL == nil {
				return fmt.Errorf("network.services.%s: missing field `url`", name)
			}
		}
	}
	return nil
}

// ActorDriver is what runs an actor.
type ActorDriver string

// The actor drivers.
const (
	// ActorDriverLinuxFirecracker is a Firecracker microVM; needs `/dev/kvm`.
	ActorDriverLinuxFirecracker ActorDriver = "linux-firecracker"
	// ActorDriverLinuxContainer is an OCI container through a
	// Docker-compatible engine.
	ActorDriverLinuxContainer ActorDriver = "linux-container"
	// ActorDriverMacosVz is a Virtualization.framework VM through vfkit.
	ActorDriverMacosVz ActorDriver = "macos-vz"
	// ActorDriverMacosAppleContainer is a container under Apple's
	// `container`.
	ActorDriverMacosAppleContainer ActorDriver = "macos-apple-container"
)

// UnmarshalJSON reads a known actor driver.
func (d *ActorDriver) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, d, "actor driver",
		ActorDriverLinuxFirecracker,
		ActorDriverLinuxContainer,
		ActorDriverMacosVz,
		ActorDriverMacosAppleContainer,
	)
}

// Seed is a stable machine identity seed: a non-negative integer or a
// string label. Exactly one of its fields is set.
type Seed struct {
	// Int is the seed written as an integer.
	Int *uint64
	// String is the seed written as a string label.
	String *string
}

// MarshalJSON writes the seed in its written form.
func (s Seed) MarshalJSON() ([]byte, error) {
	switch {
	case s.Int != nil && s.String == nil:
		return []byte(strconv.FormatUint(*s.Int, 10)), nil
	case s.String != nil && s.Int == nil:
		return json.Marshal(*s.String)
	}
	return nil, errors.New("a seed sets exactly one of Int and String")
}

// UnmarshalJSON reads an integer or a string seed.
func (s *Seed) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*s = Seed{String: &text}
		return nil
	case '0':
		var n uint64
		if err := json.Unmarshal(data, &n); err == nil {
			*s = Seed{Int: &n}
			return nil
		}
	}
	return fmt.Errorf("invalid seed %s: expected a non-negative integer or a string", data)
}

// Workspace is the actor's workspace: one directory, from which needy
// derives the file mappings and path map that make it appear in the actor.
type Workspace struct {
	// Path is the directory in the actor; `~` is the profile user's home.
	Path *string `json:"path,omitzero"`
	// From is `executor:NAME:PATH`, `host:PATH`, or absent for a fresh
	// directory.
	From *Side `json:"from,omitzero"`
	// Writable lets the actor change the workspace's source.
	Writable *bool `json:"writable,omitzero"`
}

// ContainerActor holds a container actor's options.
type ContainerActor struct {
	// OCIRuntime is the OCI runtime the engine runs the container with.
	OCIRuntime *OCIRuntime `json:"oci_runtime,omitzero"`
}

// OCIRuntime is a container actor's OCI runtime, by Docker's name for it.
type OCIRuntime string

// The OCI runtimes.
const (
	// OCIRuntimeRunc is namespaces and cgroups on the host's kernel.
	OCIRuntimeRunc OCIRuntime = "runc"
	// OCIRuntimeRunsc is gVisor, a user-space kernel.
	OCIRuntimeRunsc OCIRuntime = "runsc"
)

// UnmarshalJSON reads a known OCI runtime.
func (r *OCIRuntime) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, r, "OCI runtime", OCIRuntimeRunc, OCIRuntimeRunsc)
}

// Limits restricts the actor's driving program.
type Limits struct {
	// SingleProcess limits the driving program to one process.
	SingleProcess *bool `json:"single_process,omitzero"`
	// SelfReplace lets the driving program replace itself.
	SelfReplace *bool `json:"self_replace,omitzero"`
}
