package profile

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Chaos holds named, deterministic faults.
type Chaos struct {
	// Seed is mixed with each fault's name and match count, so a replay with
	// the same seed fires the same faults.
	Seed *uint64 `json:"seed,omitzero"`
	// Faults are the faults by name, checked in name order.
	Faults map[string]Fault `json:"faults,omitzero"`
}

// Fault is one fault: exactly one of Exec, File, Net and Sensor, with only
// that domain's effects and triggers.
type Fault struct {
	// Exec is a command pattern, written like a rule's `match`.
	Exec *string `json:"exec,omitzero"`
	// File matches files the actor opens.
	File *FileMatch `json:"file,omitzero"`
	// Net matches connections or plain-HTTP requests at the actor gateway.
	Net *NetMatch `json:"net,omitzero"`
	// Sensor holds the readings a sensor fault sets.
	Sensor *Readings `json:"sensor,omitzero"`

	// Exit makes the command exit with this code without running (exec).
	Exit *int32 `json:"exit,omitzero"`
	// Signal makes the command die of this signal without running (exec).
	Signal *string `json:"signal,omitzero"`
	// Stderr is written to the command's stderr before its end (exec).
	Stderr *string `json:"stderr,omitzero"`
	// Errno fails the open with this errno (file).
	Errno *string `json:"errno,omitzero"`
	// Refuse refuses the connection (net).
	Refuse *bool `json:"refuse,omitzero"`
	// Reset closes the connection without an answer (net).
	Reset *bool `json:"reset,omitzero"`
	// Status answers a plain-HTTP request with this status (net).
	Status *uint16 `json:"status,omitzero"`
	// Hang never completes the operation (exec and net).
	Hang *bool `json:"hang,omitzero"`
	// Delay holds the operation this long first (exec, file and net).
	Delay *Duration `json:"delay,omitzero"`

	// Probability fires the fault with this probability on each match.
	Probability *float64 `json:"probability,omitzero"`
	// Nth fires the fault only on this match, counted from 1.
	Nth *uint64 `json:"nth,omitzero"`
	// Times fires the fault at most this many times.
	Times *uint64 `json:"times,omitzero"`
	// After matches only this long after the run was created, or later.
	After *Duration `json:"after,omitzero"`
	// Until matches only until this long after the run was created.
	Until *Duration `json:"until,omitzero"`
	// At is when a sensor fault sets its readings, after the actor starts.
	At *Duration `json:"at,omitzero"`
}

// FileMatch is what a file fault matches.
type FileMatch struct {
	// Open is a glob on the path the actor opens.
	Open *string `json:"open,omitzero"`
}

// NetMatch is what a net fault matches: exactly one of its fields.
type NetMatch struct {
	// Connect matches every connection to a matching host.
	Connect *string `json:"connect,omitzero"`
	// Request matches plain-HTTP requests to a matching host only.
	Request *string `json:"request,omitzero"`
}

// Readings are the sensor readings a sensor fault sets.
type Readings struct {
	// TempC is the package temperature in degrees Celsius.
	TempC *float64 `json:"temp_c,omitzero"`
}

// FileErrnos are the errnos a file fault can deliver.
var FileErrnos = []string{
	"ENOENT",
	"EACCES",
	"EPERM",
	"EOPNOTSUPP",
	"EIO",
}

// Signals are the signals an exec fault may end a command with.
var Signals = []string{
	"SIGHUP",
	"SIGINT",
	"SIGQUIT",
	"SIGILL",
	"SIGTRAP",
	"SIGABRT",
	"SIGBUS",
	"SIGFPE",
	"SIGKILL",
	"SIGUSR1",
	"SIGSEGV",
	"SIGUSR2",
	"SIGPIPE",
	"SIGALRM",
	"SIGTERM",
	"SIGSTKFLT",
	"SIGXCPU",
	"SIGXFSZ",
	"SIGVTALRM",
	"SIGPROF",
	"SIGIO",
	"SIGPWR",
	"SIGSYS",
}

const (
	faultDelay = "delay"
	faultHang  = "hang"
)

const (
	domainExec   = "exec"
	domainFile   = "file"
	domainNet    = "net"
	domainSensor = "sensor"
)

var faultTriggers = []string{
	"probability",
	"nth",
	"times",
	"after",
	"until",
}

var faultAllowed = map[string][]string{
	domainExec:   append([]string{"exit", "signal", "stderr", faultHang, faultDelay}, faultTriggers...),
	domainFile:   append([]string{"errno", faultDelay}, faultTriggers...),
	domainNet:    append([]string{"refuse", "reset", "status", faultHang, faultDelay}, faultTriggers...),
	domainSensor: {"at"},
}

var faultOffers = map[string]string{
	domainExec: "an exec fault takes exit, signal, hang or delay (and stderr), " +
		"with probability, nth, times, after or until",
	domainFile: "a file fault takes errno or delay, with probability, nth, times, after or until",
	domainNet: "a net fault takes refuse, reset, hang, status or delay, " +
		"with probability, nth, times, after or until",
	domainSensor: "a sensor fault takes only `at`",
}

// Domain is the domain the fault applies in: `exec`, `file`, `net` or
// `sensor`.
func (f Fault) Domain() string {
	switch {
	case f.Exec != nil:
		return domainExec
	case f.File != nil:
		return domainFile
	case f.Net != nil:
		return domainNet
	}
	return domainSensor
}

func isTrue(b *bool) bool {
	return b != nil && *b
}

// setKeys are the effect and trigger keys the fault sets.
func (f Fault) setKeys() []string {
	keys := []struct {
		name string
		set  bool
	}{
		{"exit", f.Exit != nil},
		{"signal", f.Signal != nil},
		{"stderr", f.Stderr != nil},
		{"errno", f.Errno != nil},
		{"refuse", isTrue(f.Refuse)},
		{"reset", isTrue(f.Reset)},
		{"status", f.Status != nil},
		{faultHang, isTrue(f.Hang)},
		{faultDelay, f.Delay != nil},
		{"probability", f.Probability != nil},
		{"nth", f.Nth != nil},
		{"times", f.Times != nil},
		{"after", f.After != nil},
		{"until", f.Until != nil},
		{"at", f.At != nil},
	}
	var set []string
	for _, key := range keys {
		if key.set {
			set = append(set, key.name)
		}
	}
	return set
}

// Check checks the fault as needy does once a profile is merged: one
// domain, only that domain's effects and triggers, and their values.
func (f Fault) Check() error {
	var domains []string
	for _, domain := range []struct {
		name string
		set  bool
	}{
		{domainExec, f.Exec != nil},
		{domainFile, f.File != nil},
		{domainNet, f.Net != nil},
		{domainSensor, f.Sensor != nil},
	} {
		if domain.set {
			domains = append(domains, domain.name)
		}
	}
	switch len(domains) {
	case 0:
		return errors.New("a fault names one domain: `exec`, `file`, `net` or `sensor`")
	case 1:
	default:
		return fmt.Errorf("a fault names one domain, not %s", strings.Join(domains, " and "))
	}
	domain := f.Domain()
	for _, key := range f.setKeys() {
		if !slices.Contains(faultAllowed[domain], key) {
			return fmt.Errorf("%s is not for %s faults; %s", key, domain, faultOffers[domain])
		}
	}
	var err error
	switch domain {
	case domainExec:
		err = f.checkExec()
	case domainFile:
		err = f.checkFile()
	case domainNet:
		err = f.checkNet()
	}
	if err != nil {
		return err
	}
	return f.checkTriggers()
}

func (f Fault) checkTriggers() error {
	if p := f.Probability; p != nil && (*p < 0 || *p > 1) {
		return fmt.Errorf("probability %v is not between 0 and 1", *p)
	}
	if f.Nth != nil && *f.Nth == 0 {
		return errors.New("nth counts matches from 1")
	}
	if f.Times != nil && *f.Times == 0 {
		return errors.New("times: 0 would never fire")
	}
	if f.Nth != nil && (f.Probability != nil || f.Times != nil) {
		return errors.New("nth fires on one match; it takes no probability or times")
	}
	if f.After != nil && f.Until != nil {
		after, errAfter := durationNanos(*f.After)
		until, errUntil := durationNanos(*f.Until)
		if errAfter == nil && errUntil == nil && !after.less(until) {
			return errors.New("until must be later than after")
		}
	}
	return nil
}

type nanos struct {
	secs  uint64
	nanos uint32
}

func (a nanos) less(b nanos) bool {
	return a.secs < b.secs || (a.secs == b.secs && a.nanos < b.nanos)
}

func durationNanos(d Duration) (nanos, error) {
	var n nanos
	var err error
	if d.number {
		n.secs, n.nanos, err = durationFromNumber(d.text)
	} else {
		n.secs, n.nanos, err = durationFromString(d.text)
	}
	return n, err
}

func (f Fault) checkExec() error {
	ends := 0
	for _, set := range []bool{f.Exit != nil, f.Signal != nil, isTrue(f.Hang)} {
		if set {
			ends++
		}
	}
	if ends > 1 {
		return errors.New("an exec fault ends the command one way: exit, signal or hang")
	}
	if f.Stderr != nil && ends == 0 {
		return errors.New("stderr is written before exit, signal or hang; add one")
	}
	if ends == 0 && f.Delay == nil {
		return fmt.Errorf("an exec fault needs an effect; %s", faultOffers[domainExec])
	}
	if f.Exit != nil && (*f.Exit < 0 || *f.Exit > 255) {
		return fmt.Errorf("exit %d is not an exit status (0 to 255)", *f.Exit)
	}
	if f.Signal != nil {
		name := *f.Signal
		if !strings.HasPrefix(name, "SIG") {
			name = "SIG" + name
		}
		if !slices.Contains(Signals, name) {
			return fmt.Errorf("signal %s does not end a process; one of %s", *f.Signal, strings.Join(Signals, ", "))
		}
	}
	if strings.TrimSpace(*f.Exec) == "" {
		return errors.New("exec needs a command pattern, such as `curl **` (`**` for every command)")
	}
	return nil
}

func (f Fault) checkFile() error {
	if f.File.Open == nil {
		return errors.New("file: missing field `open`")
	}
	if f.Errno == nil && f.Delay == nil {
		return fmt.Errorf("a file fault needs an effect; %s", faultOffers[domainFile])
	}
	if f.Errno != nil && !slices.Contains(FileErrnos, *f.Errno) {
		return fmt.Errorf(
			"errno %s cannot reach the program: sshfs turns every other SFTP status into EIO; one of %s",
			*f.Errno,
			strings.Join(FileErrnos, ", "),
		)
	}
	return nil
}

func (f Fault) checkNet() error {
	switch {
	case f.Net.Connect != nil && f.Net.Request != nil:
		return errors.New("net matches `connect` or `request`, not both")
	case f.Net.Connect == nil && f.Net.Request == nil:
		return errors.New("net needs `connect: HOST` or `request: HOST`")
	}
	ends := 0
	for _, set := range []bool{isTrue(f.Refuse), isTrue(f.Reset), isTrue(f.Hang), f.Status != nil} {
		if set {
			ends++
		}
	}
	if ends > 1 {
		return errors.New("a net fault fails the connection one way: refuse, reset, hang or status")
	}
	if ends == 0 && f.Delay == nil {
		return fmt.Errorf("a net fault needs an effect; %s", faultOffers[domainNet])
	}
	if f.Status != nil {
		if f.Net.Request == nil {
			return errors.New("status answers a plain-HTTP request; match it with `net: { request: HOST }`")
		}
		if *f.Status < 100 || *f.Status > 599 {
			return fmt.Errorf("status %d is not an HTTP status", *f.Status)
		}
	}
	return nil
}
