// Package profile types needy's run profile schema (needy v0.0.20, unchanged through v0.0.22), so a
// profile can be decoded, changed and encoded again without losing what
// its author wrote.
//
// Every key keeps its presence: an optional value is a pointer, and a list
// or a map is nil when absent and non-nil (possibly empty) when written. A
// profile with `extends` merges over its parent key by key, so a key
// written with needy's default value still overrides the parent, and an
// absent one does not; re-encoding never adds a key that was absent. A key
// written as null, which deletes the parent's value, is kept in
// [Profile.Nulls].
//
// Decode checks what needy checks while it reads a document: unknown keys,
// value types and forms, enumerations, and each value's own rules. A
// profile without `extends` is complete, so Decode also checks its
// required keys and its faults; a profile with `extends` is checked for
// those only once needy merges it with its parents. Cross-references
// (rules naming executors, file mappings naming runs) are `needy config
// profile check`'s and are not checked here.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Profile is one run: machines, routing rules, networks, files and chaos.
type Profile struct {
	// Extends names the parent profile: `builtin:NAME` or a path relative to
	// this profile.
	Extends *string `json:"extends,omitzero"`
	// User is the single user the actor and every executor share.
	User *User `json:"user,omitzero"`
	// Actor is the machine where the driving program runs.
	Actor *Actor `json:"actor,omitzero"`
	// Executors are where routed commands run, by name.
	Executors map[string]Executor `json:"executors,omitzero"`
	// ExecutorNetwork is the executors' network policy.
	ExecutorNetwork *NetworkPolicy `json:"executor_network,omitzero"`
	// Presets are named blocks of actions that rules reuse with `use:`.
	Presets map[string]Actions `json:"presets,omitzero"`
	// Commands are the command rules; every rule that matches applies, in
	// order.
	Commands []Rule `json:"commands,omitzero"`
	// Files are the named file mappings.
	Files *Files `json:"files,omitzero"`
	// RunFiles are the mappings this run uses; empty means all of them.
	RunFiles []string `json:"run_files,omitzero"`
	// Chaos holds the named, deterministic faults.
	Chaos *Chaos `json:"chaos,omitzero"`
	// Resolvers are the launcher resolvers.
	Resolvers []Resolver `json:"resolvers,omitzero"`
	// Unknown places launchers whose effective command is unknown.
	Unknown *UnknownPolicy `json:"unknown,omitzero"`
	// DefaultExecutor is where commands no rule places run; the actor when
	// absent.
	DefaultExecutor *string `json:"default_executor,omitzero"`
	// Trace is what the run's trace records and where it goes.
	Trace *Trace `json:"trace,omitzero"`

	// Nulls are the mapping keys written with a null value, which delete the
	// parent's value along `extends`; Encode writes each one back unless the
	// key has a value again.
	Nulls []Path `json:"-"`
}

// User is the single user that the actor and every executor share.
type User struct {
	// Name is the user's name; `agent` when absent.
	Name *string `json:"name,omitzero"`
	// UID is the user's ID; 1000 when absent.
	UID *uint32 `json:"uid,omitzero"`
	// GID is the user's group ID; 1000 when absent.
	GID *uint32 `json:"gid,omitzero"`
}

// Decode reads a profile document as needy reads one before merging its
// `extends` chain, refusing unknown keys and values of the wrong form.
func Decode(data []byte) (*Profile, error) {
	full, stripped, nulls, err := yamlToJSON(data)
	if err != nil {
		return nil, err
	}
	// NOTICE: the document is decoded twice. With its null entries, so that
	// an unknown key is refused even when its value is null; and without
	// them, so that a null map entry does not become an empty value.
	var check Profile
	if err := strictUnmarshal(full, &check); err != nil {
		return nil, fmt.Errorf("invalid profile: %w", err)
	}
	var p Profile
	if err := strictUnmarshal(stripped, &p); err != nil {
		return nil, fmt.Errorf("invalid profile: %w", err)
	}
	p.Nulls = nulls
	if p.Extends == nil {
		if err := p.checkComplete(); err != nil {
			return nil, fmt.Errorf("invalid profile: %w", err)
		}
	}
	return &p, nil
}

// Encode writes the profile as YAML.
func (p *Profile) Encode() ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return jsonToYAML(data, p.Nulls)
}

// checkComplete checks what needy checks once a profile's `extends` chain is
// merged: required keys of mappings that merge key by key, and faults.
func (p *Profile) checkComplete() error {
	if p.Actor == nil {
		return errors.New("missing field `actor`")
	}
	if err := p.Actor.checkComplete(); err != nil {
		return fmt.Errorf("actor.%w", err)
	}
	if p.Files != nil {
		for _, name := range sortedKeys(p.Files.Mappings) {
			if err := p.Files.Mappings[name].checkComplete(); err != nil {
				return fmt.Errorf("files.%s.%w", name, err)
			}
		}
	}
	if p.Chaos != nil {
		for _, name := range sortedKeys(p.Chaos.Faults) {
			if err := p.Chaos.Faults[name].Check(); err != nil {
				return fmt.Errorf("chaos.faults.%s: %w", name, err)
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// unmarshalEnum reads a string that must be one of allowed.
func unmarshalEnum[T ~string](data []byte, target *T, what string, allowed ...T) error {
	if isJSONNull(data) {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("invalid %s %s: expected a string", what, bytes.TrimSpace(data))
	}
	if !slices.Contains(allowed, T(text)) {
		names := make([]string, 0, len(allowed))
		for _, value := range allowed {
			names = append(names, fmt.Sprintf("`%s`", value))
		}
		return fmt.Errorf("unknown %s %q, expected one of %s", what, text, joinNames(names))
	}
	*target = T(text)
	return nil
}

func joinNames(names []string) string {
	var b bytes.Buffer
	for i, name := range names {
		switch {
		case i == 0:
		case i == len(names)-1:
			b.WriteString(" or ")
		default:
			b.WriteString(", ")
		}
		b.WriteString(name)
	}
	return b.String()
}
