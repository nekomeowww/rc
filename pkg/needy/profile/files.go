package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const ignoreFileKey = "ignore_file"

// Files are the profile's named file mappings, written as one mapping whose
// keys other than `ignore_file` name mappings.
type Files struct {
	// IgnoreFile is an optional gitignore-style file whose lines become `-`
	// filter rules.
	IgnoreFile *string
	// Mappings are the file mappings by name.
	Mappings map[string]FileMapping
}

// MarshalJSON writes `ignore_file` and then each mapping by name.
func (f Files) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	write := func(key string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(quoteJSON(key))
		b.WriteByte(':')
		b.Write(data)
		return nil
	}
	if f.IgnoreFile != nil {
		if err := write(ignoreFileKey, *f.IgnoreFile); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeys(f.Mappings) {
		if name == ignoreFileKey {
			return nil, errors.New("a file mapping cannot be named ignore_file")
		}
		if err := write(name, f.Mappings[name]); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads `ignore_file` and the mappings beside it.
func (f *Files) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("invalid files: expected file mappings by name: %w", err)
	}
	files := Files{Mappings: map[string]FileMapping{}}
	for name, raw := range object {
		if name == ignoreFileKey {
			if isJSONNull(raw) {
				continue
			}
			var ignore string
			if err := json.Unmarshal(raw, &ignore); err != nil {
				return fmt.Errorf("files.ignore_file: %w", err)
			}
			files.IgnoreFile = &ignore
			continue
		}
		if isJSONNull(raw) {
			continue
		}
		var mapping FileMapping
		if err := strictUnmarshal(raw, &mapping); err != nil {
			return fmt.Errorf("files.%s: %w", name, err)
		}
		files.Mappings[name] = mapping
	}
	*f = files
	return nil
}

// FileMapping is one named copy-in, mount, mirror-out or sync between the
// host, the actor and executors.
type FileMapping struct {
	// Mode is how the files move.
	Mode *FileMode `json:"mode,omitzero"`
	// From is the source side.
	From *Side `json:"from,omitzero"`
	// To is the destination side.
	To *Side `json:"to,omitzero"`
	// Filter holds rclone-style ordered `+` and `-` rules; first match wins.
	Filter []string `json:"filter,omitzero"`
	// Writable lets the destination change the source.
	Writable *bool `json:"writable,omitzero"`
	// When is when a mirror-out copies; `end` when absent.
	When *MirrorWhen `json:"when,omitzero"`
	// Delete propagates deletions to the host.
	Delete *bool `json:"delete,omitzero"`
	// OnConflict resolves entries a sync mapping's run and store both
	// changed, by pattern, first match first.
	OnConflict OnConflict `json:"on-conflict,omitzero"`
}

func (m FileMapping) checkComplete() error {
	switch {
	case m.Mode == nil:
		return errors.New("missing field `mode`")
	case m.From == nil:
		return errors.New("missing field `from`")
	case m.To == nil:
		return errors.New("missing field `to`")
	}
	return nil
}

// FileMode is how a file mapping moves files.
type FileMode string

// The file mapping modes.
const (
	// FileModeCopyIn copies the source in when the run starts.
	FileModeCopyIn FileMode = "copy-in"
	// FileModeMount mounts the source live.
	FileModeMount FileMode = "mount"
	// FileModeMirrorOut copies the source out.
	FileModeMirrorOut FileMode = "mirror-out"
	// FileModeSync copies a store in and merges it back three-way at the end.
	FileModeSync FileMode = "sync"
)

// UnmarshalJSON reads a known file mapping mode.
func (m *FileMode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, m, "file mode", FileModeCopyIn, FileModeMount, FileModeMirrorOut, FileModeSync)
}

// MirrorWhen is when a mirror-out copies.
type MirrorWhen string

// The mirror-out times.
const (
	// MirrorWhenContinuous copies as files change.
	MirrorWhenContinuous MirrorWhen = "continuous"
	// MirrorWhenEnd copies when the run ends.
	MirrorWhenEnd MirrorWhen = "end"
)

// UnmarshalJSON reads a known mirror-out time.
func (w *MirrorWhen) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, w, "mirror time", MirrorWhenContinuous, MirrorWhenEnd)
}

// OnConflict is a sync mapping's ordered conflict rules, written as a
// mapping from patterns to choices.
type OnConflict []OnConflictRule

// OnConflictRule is one pattern and what resolves the entries it matches.
type OnConflictRule struct {
	// Pattern matches entries as a `filter` rule's pattern does.
	Pattern string
	// Choice resolves the matched entries.
	Choice ConflictChoice
}

// ConflictChoice resolves a sync conflict: `store`, `run`, `lines`, or a
// merge driver command. Exactly one of its fields is set.
type ConflictChoice struct {
	// Name is `store`, `run` or `lines`.
	Name string
	// Driver is a merge driver command, as written.
	Driver *string
}

// The named conflict choices.
const (
	// ConflictStore keeps the store's copy.
	ConflictStore = "store"
	// ConflictRun takes the run's copy.
	ConflictRun = "run"
	// ConflictLines appends the lines the run appended.
	ConflictLines = "lines"
)

// MarshalJSON writes the rules as a mapping in their order.
func (o OnConflict) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, rule := range o {
		value, err := json.Marshal(rule.Choice)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteJSON(rule.Pattern))
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads a mapping from patterns to choices, keeping its order.
func (o *OnConflict) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New(
			"invalid on-conflict: expected a map from patterns to `store`, `run`, `lines` or `{ driver: COMMAND }`",
		)
	}
	rules := OnConflict{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		pattern, _ := keyToken.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if isJSONNull(raw) {
			continue
		}
		var choice ConflictChoice
		if err := choice.UnmarshalJSON(raw); err != nil {
			return fmt.Errorf("on-conflict.%s: %w", pattern, err)
		}
		rules = append(rules, OnConflictRule{Pattern: pattern, Choice: choice})
	}
	*o = rules
	return nil
}

// Command is the merge driver command as needy runs it, trimmed.
func (c ConflictChoice) Command() string {
	if c.Driver == nil {
		return ""
	}
	return strings.TrimSpace(*c.Driver)
}

// MarshalJSON writes a choice name or `{ driver: COMMAND }`.
func (c ConflictChoice) MarshalJSON() ([]byte, error) {
	if c.Driver != nil {
		if c.Name != "" {
			return nil, errors.New("a conflict choice sets exactly one of Name and Driver")
		}
		return json.Marshal(map[string]string{"driver": *c.Driver})
	}
	switch c.Name {
	case ConflictStore, ConflictRun, ConflictLines:
		return json.Marshal(c.Name)
	}
	return nil, fmt.Errorf("unknown on-conflict choice %q", c.Name)
}

// UnmarshalJSON reads `store`, `run`, `lines` or `{ driver: COMMAND }`.
func (c *ConflictChoice) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		// A null choice deletes the parent's along `extends`; it is kept in
		// Profile.Nulls.
		return nil
	case '"':
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		switch name {
		case ConflictStore, ConflictRun, ConflictLines:
			*c = ConflictChoice{Name: name}
			return nil
		}
		return fmt.Errorf("unknown on-conflict choice %q: use `store`, `run`, `lines` or `{ driver: COMMAND }`", name)
	case '{':
		var driver struct {
			Driver *string `json:"driver"`
		}
		if err := strictUnmarshal(data, &driver); err != nil {
			return err
		}
		if driver.Driver == nil {
			return errors.New("missing field `driver`")
		}
		if strings.TrimSpace(*driver.Driver) == "" {
			return errors.New("merge driver command must not be empty")
		}
		*c = ConflictChoice{Driver: driver.Driver}
		return nil
	}
	return errors.New("expected `store`, `run`, `lines` or `{ driver: COMMAND }`")
}

// Side is one side of a file mapping: `host:PATH`, `actor:PATH` or
// `executor:NAME:PATH`, kept as written.
type Side string

// SideKind is where a side's path is.
type SideKind string

// The side kinds.
const (
	// SideHost is a path on the control host.
	SideHost SideKind = "host"
	// SideActor is a path in the actor.
	SideActor SideKind = "actor"
	// SideExecutor is a path on an executor.
	SideExecutor SideKind = "executor"
)

// Parse splits the side into its kind, executor name and path.
func (s Side) Parse() (SideKind, string, string, error) {
	text := string(s)
	if path, ok := strings.CutPrefix(text, "host:"); ok {
		return SideHost, "", path, nil
	}
	if path, ok := strings.CutPrefix(text, "actor:"); ok {
		return SideActor, "", path, nil
	}
	if rest, ok := strings.CutPrefix(text, "executor:"); ok {
		name, path, found := strings.Cut(rest, ":")
		if !found {
			return "", "", "", fmt.Errorf("executor side needs NAME:PATH: %q", text)
		}
		if name == "" {
			return "", "", "", fmt.Errorf("executor side needs a name: %q", text)
		}
		return SideExecutor, name, path, nil
	}
	return "", "", "", fmt.Errorf("file side must start with host:, actor: or executor:: %q", text)
}

// UnmarshalJSON reads a side string.
func (s *Side) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if _, _, _, err := Side(text).Parse(); err != nil {
		return err
	}
	*s = Side(text)
	return nil
}
