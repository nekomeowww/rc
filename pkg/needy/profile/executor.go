package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
)

// Executor is where routed commands run, and how its instances are pooled.
type Executor struct {
	// Runner is the runner the executor runs on; the control host when
	// absent.
	Runner *RunnerSelector `json:"runner,omitzero"`
	// Driver is what the runner creates for it; `native` when absent.
	Driver *ExecutorDriver `json:"driver,omitzero"`
	// Container is the container's image and run options, for
	// `linux-container`.
	Container *ContainerExecutor `json:"container,omitzero"`
	// Recipe is a recipe reference, for drivers that build a machine.
	Recipe *string `json:"recipe,omitzero"`
	// Workspace is the working directory of a command that names none.
	Workspace *string `json:"workspace,omitzero"`
	// PathMap maps actor path prefixes to executor path prefixes.
	PathMap map[string]string `json:"path_map,omitzero"`
	// Key groups commands onto one instance: `{run}`, `{profile}`, `{group}`,
	// `{rule}`; `{run}` when absent.
	Key *string `json:"key,omitzero"`
	// MaxProcs is the matched commands running at once per instance.
	MaxProcs *uint32 `json:"max_procs,omitzero"`
	// OnFull is what a command does when its instance has no free slot.
	OnFull *OnFull `json:"on_full,omitzero"`
	// MaxInstances caps the executor's instances.
	MaxInstances *uint32 `json:"max_instances,omitzero"`
	// IdleTimeout is how long an idle instance lives.
	IdleTimeout *Duration `json:"idle_timeout,omitzero"`
	// UnavailableTimeout is the longest pre-dispatch wait for a disconnected
	// executor; 10s when absent.
	UnavailableTimeout *Duration `json:"unavailable_timeout,omitzero"`
}

// ExecutorDriver is what creates an executor's instance on its runner.
type ExecutorDriver string

// The executor drivers.
const (
	// ExecutorDriverNative is the runner's own process space.
	ExecutorDriverNative ExecutorDriver = "native"
	// ExecutorDriverLinuxContainer is a container the runner's engine
	// creates.
	ExecutorDriverLinuxContainer ExecutorDriver = "linux-container"
)

// UnmarshalJSON reads a known executor driver.
func (d *ExecutorDriver) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, d, "executor driver", ExecutorDriverNative, ExecutorDriverLinuxContainer)
}

// OnFull is what a command does when its instance has no free slot.
type OnFull string

// The on-full policies.
const (
	// OnFullWait waits for a free slot.
	OnFullWait OnFull = "wait"
	// OnFullReject refuses the command.
	OnFullReject OnFull = "reject"
)

// UnmarshalJSON reads a known on-full policy.
func (o *OnFull) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, o, "on_full policy", OnFullWait, OnFullReject)
}

// ContainerExecutor is a `linux-container` executor's image and run options.
type ContainerExecutor struct {
	// Image is the container's image.
	Image *string `json:"image,omitzero"`
	// Build is how to build the container's image.
	Build *ContainerBuild `json:"build,omitzero"`
	// Cpus is the CPUs the container may use, as `docker run --cpus`.
	Cpus *Cpus `json:"cpus,omitzero"`
	// CPUShares is the relative CPU weight.
	CPUShares *int64 `json:"cpu_shares,omitzero"`
	// CpusetCpus is the CPUs the container may run on: `0-3`, `0,1`.
	CpusetCpus *string `json:"cpuset_cpus,omitzero"`
	// Memory is the container's memory limit.
	Memory *ByteSize `json:"memory,omitzero"`
	// MemorySwap is memory plus swap, or `unlimited`.
	MemorySwap *MemorySwap `json:"memory_swap,omitzero"`
	// ShmSize is the size of `/dev/shm`.
	ShmSize *ByteSize `json:"shm_size,omitzero"`
	// PidsLimit is the processes the container may hold at once; -1 for no
	// limit.
	PidsLimit *int64 `json:"pids_limit,omitzero"`
	// Ulimits are resource limits by name.
	Ulimits map[string]Ulimit `json:"ulimits,omitzero"`
	// Env is the container's environment.
	Env map[string]string `json:"env,omitzero"`
	// Workdir is the container's working directory.
	Workdir *string `json:"workdir,omitzero"`
	// ReadOnly mounts the container's root file system read-only.
	ReadOnly *bool `json:"read_only,omitzero"`
	// Init runs an init process as PID 1.
	Init *bool `json:"init,omitzero"`
	// Mounts are mounts besides the workspace and the daemon's socket.
	Mounts []Mount `json:"mounts,omitzero"`
	// AddHost maps host names to addresses in `/etc/hosts`.
	AddHost map[string]string `json:"add_host,omitzero"`
	// DNS lists DNS server IP addresses.
	DNS []IPAddr `json:"dns,omitzero"`
	// Sysctls are namespaced kernel parameters.
	Sysctls map[string]string `json:"sysctls,omitzero"`
	// Gpus are the container's GPUs; needs the operator.
	Gpus *Gpus `json:"gpus,omitzero"`
	// CapDrop lists capabilities to drop.
	CapDrop []string `json:"cap_drop,omitzero"`
	// Privileged runs the container privileged; needs the operator.
	Privileged *bool `json:"privileged,omitzero"`
	// CapAdd lists capabilities to add; needs the operator.
	CapAdd []string `json:"cap_add,omitzero"`
	// Devices are host devices; needs the operator.
	Devices []Device `json:"devices,omitzero"`
	// Pid is the PID namespace.
	Pid *string `json:"pid,omitzero"`
	// Ipc is the IPC namespace.
	Ipc *string `json:"ipc,omitzero"`
	// Userns is the user namespace.
	Userns *string `json:"userns,omitzero"`
	// Uts is the UTS namespace.
	Uts *string `json:"uts,omitzero"`
	// SecurityOpt lists security options.
	SecurityOpt []string `json:"security_opt,omitzero"`
}

// IPAddr is an IP address as written.
type IPAddr string

// UnmarshalJSON reads an IPv4 or IPv6 address.
func (a *IPAddr) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(text)
	if err != nil || addr.Zone() != "" {
		return fmt.Errorf("invalid IP address syntax %q", text)
	}
	*a = IPAddr(text)
	return nil
}

// Cpus is CPUs as `docker run --cpus` takes them: a number above 0.
type Cpus struct {
	literal string
}

// NewCpus is n CPUs.
func NewCpus(n float64) (Cpus, error) {
	if err := checkCpus(n); err != nil {
		return Cpus{}, err
	}
	return Cpus{literal: strconv.FormatFloat(n, 'f', -1, 64)}, nil
}

// Value is the number of CPUs.
func (c Cpus) Value() float64 {
	f, _ := strconv.ParseFloat(c.literal, 64)
	return f
}

// MarshalJSON writes the number as written.
func (c Cpus) MarshalJSON() ([]byte, error) {
	if c.literal == "" {
		return nil, errors.New("cpus is unset")
	}
	return []byte(c.literal), nil
}

// UnmarshalJSON reads a number of CPUs above 0.
func (c *Cpus) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	if jsonKind(data) != '0' {
		return fmt.Errorf("invalid cpus %s: expected a number of CPUs above 0, such as 1.5", data)
	}
	literal := string(bytes.TrimSpace(data))
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return err
	}
	if err := checkCpus(f); err != nil {
		return err
	}
	c.literal = literal
	return nil
}

func checkCpus(n float64) error {
	if math.IsInf(n, 0) || math.IsNaN(n) || n <= 0 || n >= 1e9 {
		return fmt.Errorf("invalid cpus %v: expected a number above 0, such as 1.5", n)
	}
	return nil
}

// MemorySwap is memory plus swap: a size, or `unlimited`. Exactly one of
// its fields is set.
type MemorySwap struct {
	// Limit is the memory plus swap limit.
	Limit *ByteSize
	// Unlimited sets no swap limit.
	Unlimited bool
}

const unlimited = "unlimited"

// MarshalJSON writes a size or `unlimited`.
func (m MemorySwap) MarshalJSON() ([]byte, error) {
	switch {
	case m.Unlimited && m.Limit == nil:
		return json.Marshal(unlimited)
	case m.Limit != nil && !m.Unlimited:
		return m.Limit.MarshalJSON()
	}
	return nil, errors.New("memory_swap sets exactly one of Limit and Unlimited")
}

// UnmarshalJSON reads a size or `unlimited`.
func (m *MemorySwap) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	if jsonKind(data) == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err == nil && text == unlimited {
			*m = MemorySwap{Unlimited: true}
			return nil
		}
	}
	var size ByteSize
	if err := size.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("%w, or unlimited", err)
	}
	*m = MemorySwap{Limit: &size}
	return nil
}

// Ulimit is one resource limit: one number for both the soft and the hard
// limit, or a pair. Exactly one of its fields is set.
type Ulimit struct {
	// Same is the soft and the hard limit.
	Same *int64
	// Pair is the soft and the hard limit apart.
	Pair *UlimitPair
}

// UlimitPair is a soft and a hard limit.
type UlimitPair struct {
	// Soft is the soft limit.
	Soft int64 `json:"soft"`
	// Hard is the hard limit.
	Hard int64 `json:"hard"`
}

// MarshalJSON writes the limit in its written form.
func (u Ulimit) MarshalJSON() ([]byte, error) {
	switch {
	case u.Same != nil && u.Pair == nil:
		return []byte(strconv.FormatInt(*u.Same, 10)), nil
	case u.Pair != nil && u.Same == nil:
		return json.Marshal(u.Pair)
	}
	return nil, errors.New("a ulimit sets exactly one of Same and Pair")
}

// UnmarshalJSON reads a limit, or `{ soft, hard }`.
func (u *Ulimit) UnmarshalJSON(data []byte) error {
	errForm := errors.New("a limit, or { soft, hard }")
	switch jsonKind(data) {
	case 'n':
		return nil
	case '0':
		var n int64
		if json.Unmarshal(data, &n) != nil {
			return errForm
		}
		*u = Ulimit{Same: &n}
		return nil
	case '{':
		var pair UlimitPair
		if strictUnmarshal(data, &pair) != nil || !hasKey(data, "soft") || !hasKey(data, "hard") {
			return errForm
		}
		*u = Ulimit{Pair: &pair}
		return nil
	}
	return errForm
}

// MountType is the kind of a container mount.
type MountType string

// The mount types.
const (
	// MountTypeBind is a host directory or file.
	MountTypeBind MountType = "bind"
	// MountTypeVolume is a Docker volume.
	MountTypeVolume MountType = "volume"
	// MountTypeTmpfs is a memory file system.
	MountTypeTmpfs MountType = "tmpfs"
)

// Mount is a container mount besides the daemon's; which fields apply
// depends on Type.
type Mount struct {
	// Type is `bind`, `volume` or `tmpfs`.
	Type MountType `json:"type"`
	// Source is a bind's host path (required) or a volume's name (anonymous
	// when absent).
	Source *string `json:"source,omitzero"`
	// Target is the path in the container.
	Target string `json:"target"`
	// ReadOnly mounts a bind or a volume read-only.
	ReadOnly *bool `json:"read_only,omitzero"`
	// Size is a tmpfs mount's size.
	Size *ByteSize `json:"size,omitzero"`
}

type bindMount struct {
	Source   *string `json:"source"`
	Target   *string `json:"target"`
	ReadOnly *bool   `json:"read_only"`
}

type volumeMount struct {
	Source   *string `json:"source"`
	Target   *string `json:"target"`
	ReadOnly *bool   `json:"read_only"`
}

type tmpfsMount struct {
	Target *string   `json:"target"`
	Size   *ByteSize `json:"size"`
}

// MarshalJSON writes the mount with its type first.
func (m Mount) MarshalJSON() ([]byte, error) {
	type plain Mount
	if err := m.check(); err != nil {
		return nil, err
	}
	return json.Marshal(plain(m))
}

// UnmarshalJSON reads a mount, refusing keys its type does not take.
func (m *Mount) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	rawType, ok := object["type"]
	if !ok || isJSONNull(rawType) {
		return errors.New("missing field `type`")
	}
	var mountType MountType
	err := unmarshalEnum(rawType, &mountType, "mount type", MountTypeBind, MountTypeVolume, MountTypeTmpfs)
	if err != nil {
		return err
	}
	delete(object, "type")
	rest, err := json.Marshal(object)
	if err != nil {
		return err
	}
	mount := Mount{Type: mountType}
	var target *string
	switch mountType {
	case MountTypeBind:
		var b bindMount
		if err := strictUnmarshal(rest, &b); err != nil {
			return err
		}
		if b.Source == nil {
			return errors.New("missing field `source`")
		}
		mount.Source, target, mount.ReadOnly = b.Source, b.Target, b.ReadOnly
	case MountTypeVolume:
		var v volumeMount
		if err := strictUnmarshal(rest, &v); err != nil {
			return err
		}
		mount.Source, target, mount.ReadOnly = v.Source, v.Target, v.ReadOnly
	case MountTypeTmpfs:
		var t tmpfsMount
		if err := strictUnmarshal(rest, &t); err != nil {
			return err
		}
		target, mount.Size = t.Target, t.Size
	}
	if target == nil {
		return errors.New("missing field `target`")
	}
	mount.Target = *target
	*m = mount
	return nil
}

func (m Mount) check() error {
	switch m.Type {
	case MountTypeBind:
		if m.Source == nil || m.Size != nil {
			return errors.New("a bind mount takes source, target and read_only")
		}
	case MountTypeVolume:
		if m.Size != nil {
			return errors.New("a volume mount takes source, target and read_only")
		}
	case MountTypeTmpfs:
		if m.Source != nil || m.ReadOnly != nil {
			return errors.New("a tmpfs mount takes target and size")
		}
	default:
		return fmt.Errorf("unknown mount type %q", m.Type)
	}
	return nil
}

// Gpus is the container's GPUs: `all`, a number of GPUs, or a list of
// device IDs. Exactly one of its fields is set.
type Gpus struct {
	// All is every GPU.
	All bool
	// Count is a number of GPUs.
	Count *uint32
	// Devices are device IDs, indexes or UUIDs.
	Devices []string
}

// MarshalJSON writes the GPUs in their written form.
func (g Gpus) MarshalJSON() ([]byte, error) {
	switch {
	case g.All && g.Count == nil && g.Devices == nil:
		return json.Marshal("all")
	case g.Count != nil && !g.All && g.Devices == nil:
		return []byte(strconv.FormatUint(uint64(*g.Count), 10)), nil
	case g.Devices != nil && !g.All && g.Count == nil:
		return json.Marshal(g.Devices)
	}
	return nil, errors.New("gpus sets exactly one of All, Count and Devices")
}

// UnmarshalJSON reads `all`, a number of GPUs or a list of device IDs.
func (g *Gpus) UnmarshalJSON(data []byte) error {
	errForm := fmt.Errorf("invalid gpus %s: expected all, a number of GPUs or a list of device IDs", data)
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var text string
		if json.Unmarshal(data, &text) != nil || text != "all" {
			return errForm
		}
		*g = Gpus{All: true}
		return nil
	case '0':
		var n uint32
		if json.Unmarshal(data, &n) != nil {
			return errForm
		}
		*g = Gpus{Count: &n}
		return nil
	case '[':
		devices := []string{}
		if err := json.Unmarshal(data, &devices); err != nil {
			return err
		}
		*g = Gpus{Devices: devices}
		return nil
	}
	return errForm
}

// Device is a host device in the container.
type Device struct {
	// Host is the device's path on the host.
	Host string `json:"host"`
	// Container is the path in the container; the host's when absent.
	Container *string `json:"container,omitzero"`
	// Permissions are cgroup permissions, some of `r`, `w` and `m`; `rwm`
	// when absent.
	Permissions *string `json:"permissions,omitzero"`
}

// UnmarshalJSON reads a device, whose `host` is required.
func (d *Device) UnmarshalJSON(data []byte) error {
	type plain struct {
		Host        *string `json:"host"`
		Container   *string `json:"container"`
		Permissions *string `json:"permissions"`
	}
	if isJSONNull(data) {
		return nil
	}
	var p plain
	if err := strictUnmarshal(data, &p); err != nil {
		return err
	}
	if p.Host == nil {
		return errors.New("missing field `host`")
	}
	*d = Device{Host: *p.Host, Container: p.Container, Permissions: p.Permissions}
	return nil
}

// ContainerBuild holds `docker build` options.
type ContainerBuild struct {
	// Context is the build context, a host path.
	Context *string `json:"context,omitzero"`
	// Dockerfile is the Dockerfile's path.
	Dockerfile *string `json:"dockerfile,omitzero"`
	// Target is the build stage.
	Target *string `json:"target,omitzero"`
	// Args are build arguments.
	Args map[string]string `json:"args,omitzero"`
}
