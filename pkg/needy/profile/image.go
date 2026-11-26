package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Image is what `actor.image` names: an OCI image reference or image file
// path (Reference), or an inline image definition (Definition). Exactly
// one of its fields is set.
type Image struct {
	// Reference is an OCI image reference, or a path to an image file that
	// needy's loader inlines.
	Reference string
	// Definition is an inline image definition.
	Definition *Definition
}

// IsFile reports whether Reference is a path to an image file: it starts
// with `./`, `../` or `/`, or ends in `.yaml`.
func (i Image) IsFile() bool {
	if i.Definition != nil {
		return false
	}
	for _, prefix := range []string{"./", "../", "/"} {
		if strings.HasPrefix(i.Reference, prefix) {
			return true
		}
	}
	return strings.HasSuffix(i.Reference, ".yaml")
}

// MarshalJSON writes a reference string or a definition mapping.
func (i Image) MarshalJSON() ([]byte, error) {
	if i.Definition != nil {
		if i.Reference != "" {
			return nil, errors.New("an image sets exactly one of Reference and Definition")
		}
		return json.Marshal(i.Definition)
	}
	if err := checkImageReference(i.Reference); err != nil {
		return nil, err
	}
	return json.Marshal(i.Reference)
}

// UnmarshalJSON reads a reference string or a definition mapping.
func (i *Image) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var reference string
		if err := json.Unmarshal(data, &reference); err != nil {
			return err
		}
		if err := checkImageReference(reference); err != nil {
			return err
		}
		*i = Image{Reference: reference}
		return nil
	case '{':
		var definition Definition
		if err := strictUnmarshal(data, &definition); err != nil {
			return err
		}
		*i = Image{Definition: &definition}
		return nil
	}
	return fmt.Errorf(
		"invalid image %s: expected an image reference, a path to an image file or an image definition",
		data,
	)
}

func checkImageReference(reference string) error {
	if strings.HasPrefix(reference, "builtin:") {
		return fmt.Errorf(
			"needy ships no built-in images, so %s names none: `builtin:` names recipes and profiles; "+
				"write an image definition and install the agent in one of its layers",
			reference,
		)
	}
	return nil
}

// Definition is an image definition: a root and the layers built over it,
// in order.
type Definition struct {
	// Root is the OCI image the layers are built over; `ubuntu:24.04` when
	// absent.
	Root *string `json:"root,omitzero"`
	// PullPolicy is when remote OCI features are pulled.
	PullPolicy *PullPolicy `json:"pull_policy,omitzero"`
	// Layers are built in order over the root; along `extends` the parent's
	// come first.
	Layers []Layer `json:"layers,omitzero"`
	// Env is the environment of the image's programs.
	Env map[string]string `json:"env,omitzero"`
	// Hooks are lifecycle hooks run in every run of this image.
	Hooks *Hooks `json:"hooks,omitzero"`
}

// PullPolicy is when remote OCI feature references are pulled.
type PullPolicy string

// The pull policies.
const (
	// PullIfNotPresent pulls only on a cache miss.
	PullIfNotPresent PullPolicy = "if-not-present"
	// PullAlways always resolves tags at the registry.
	PullAlways PullPolicy = "always"
	// PullNever never contacts a registry.
	PullNever PullPolicy = "never"
)

// UnmarshalJSON reads a known pull policy.
func (p *PullPolicy) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, p, "pull policy", PullIfNotPresent, PullAlways, PullNever)
}

// Hooks are an image's lifecycle hooks.
type Hooks struct {
	// Start runs in order before the driving program starts.
	Start []HookCommand `json:"start,omitzero"`
	// Stop runs in order after the driving program exits.
	Stop []HookCommand `json:"stop,omitzero"`
}

// HookCommand is one hook: a shell command string, or a non-empty argv
// list. Exactly one of its fields is set.
type HookCommand struct {
	// Shell is a command run by `sh -c`.
	Shell string
	// Argv is a command's words, shell-quoted for `sh -c`.
	Argv []string
}

// MarshalJSON writes the hook in its written form.
func (h HookCommand) MarshalJSON() ([]byte, error) {
	if h.Argv != nil {
		if h.Shell != "" || len(h.Argv) == 0 {
			return nil, errors.New("a hook sets a shell command or a non-empty argv")
		}
		return json.Marshal(h.Argv)
	}
	return json.Marshal(h.Shell)
}

// UnmarshalJSON reads a shell command or a non-empty argv list.
func (h *HookCommand) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case '"':
		var shell string
		if err := json.Unmarshal(data, &shell); err != nil {
			return err
		}
		*h = HookCommand{Shell: shell}
		return nil
	case '[':
		var argv []string
		if err := json.Unmarshal(data, &argv); err != nil {
			return errors.New("data did not match any variant of untagged enum RawHookCommand")
		}
		if len(argv) == 0 {
			return errors.New("hook argv array cannot be empty")
		}
		*h = HookCommand{Argv: argv}
		return nil
	}
	return errors.New("data did not match any variant of untagged enum RawHookCommand")
}

// Layer is one entry of a definition's `layers`: a `run` script, or a Dev
// Container `feature` with optional `with` options.
type Layer struct {
	// Run is a shell script run by `sh -c` as root.
	Run *string `json:"run,omitzero"`
	// Feature is a Dev Container feature reference.
	Feature *string `json:"feature,omitzero"`
	// With holds a feature's option values.
	With map[string]FeatureOption `json:"with,omitzero"`
}

// MarshalJSON writes the layer after checking its form.
func (l Layer) MarshalJSON() ([]byte, error) {
	type plain Layer
	if err := l.check(); err != nil {
		return nil, err
	}
	return json.Marshal(plain(l))
}

// UnmarshalJSON reads a `run` or a `feature` layer.
func (l *Layer) UnmarshalJSON(data []byte) error {
	type plain Layer
	if isJSONNull(data) {
		return nil
	}
	var p plain
	if err := strictUnmarshal(data, &p); err != nil {
		return err
	}
	if err := Layer(p).check(); err != nil {
		return err
	}
	*l = Layer(p)
	return nil
}

func (l Layer) check() error {
	switch {
	case l.Run != nil && l.Feature != nil:
		return errors.New("a layer is one of `run` or `feature`, not both")
	case l.Run != nil && l.With != nil:
		return errors.New("`with` sets a `feature` layer's options")
	case l.Feature != nil:
		if strings.TrimSpace(*l.Feature) == "" {
			return errors.New("`feature` reference cannot be empty")
		}
	case l.With != nil:
		return errors.New("`with` sets a `feature` layer's options")
	case l.Run == nil:
		return errors.New("a layer needs `run` or `feature`")
	}
	return nil
}

// FeatureOption is a feature option value: a string, a boolean or an
// integer. Exactly one of its fields is set.
type FeatureOption struct {
	// String is a string value.
	String *string
	// Bool is a boolean value.
	Bool *bool
	// Int is an integer value.
	Int *int64
}

// MarshalJSON writes the option's value.
func (o FeatureOption) MarshalJSON() ([]byte, error) {
	switch {
	case o.String != nil && o.Bool == nil && o.Int == nil:
		return json.Marshal(*o.String)
	case o.Bool != nil && o.String == nil && o.Int == nil:
		return json.Marshal(*o.Bool)
	case o.Int != nil && o.String == nil && o.Bool == nil:
		return []byte(strconv.FormatInt(*o.Int, 10)), nil
	}
	return nil, errors.New("a feature option sets exactly one of String, Bool and Int")
}

// UnmarshalJSON reads a string, a boolean or an integer.
func (o *FeatureOption) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*o = FeatureOption{String: &s}
		return nil
	case 't':
		var b bool
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		*o = FeatureOption{Bool: &b}
		return nil
	case '0':
		literal := strings.TrimSpace(string(data))
		if !isIntegerLiteral(literal) {
			return fmt.Errorf(
				"feature option value %s is a floating-point number; quote decimal versions as strings (e.g. \"%s\")",
				literal,
				literal,
			)
		}
		n, err := strconv.ParseInt(literal, 10, 64)
		if err != nil {
			return fmt.Errorf("feature option integer %s is out of range", literal)
		}
		*o = FeatureOption{Int: &n}
		return nil
	}
	return errors.New("expected a string, boolean, or integer feature option value")
}
