package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Rule is a command rule: a pattern, conditions and actions, written in one
// mapping.
type Rule struct {
	// Match is written like the command it matches: `git **`.
	Match string
	// When holds the conditions that are not positional.
	When *When
	// Actions are what the rule does, written beside `match` and `when`.
	Actions Actions
}

// MarshalJSON writes `match`, `when` and the actions in one mapping.
func (r Rule) MarshalJSON() ([]byte, error) {
	actions, err := json.Marshal(r.Actions)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`{"match":`)
	b.WriteString(quoteJSON(r.Match))
	if r.When != nil {
		when, err := json.Marshal(r.When)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"when":`)
		b.Write(when)
	}
	if rest := bytes.TrimPrefix(actions, []byte("{")); len(rest) > 1 {
		b.WriteByte(',')
		b.Write(rest)
	} else {
		b.WriteByte('}')
	}
	return b.Bytes(), nil
}

// UnmarshalJSON reads `match`, `when`, and every other key as an action.
func (r *Rule) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return errors.New("invalid type: null, expected a command rule")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("invalid command rule: %w", err)
	}
	var rule Rule
	raw, ok := object["match"]
	if !ok {
		return errors.New("missing field `match`")
	}
	if isJSONNull(raw) || json.Unmarshal(raw, &rule.Match) != nil {
		return fmt.Errorf("match: invalid type %s, expected a string", raw)
	}
	delete(object, "match")
	if raw, ok := object["when"]; ok {
		if !isJSONNull(raw) {
			rule.When = &When{}
			if err := strictUnmarshal(raw, rule.When); err != nil {
				return fmt.Errorf("when: %w", err)
			}
		}
		delete(object, "when")
	}
	rest, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if err := strictUnmarshal(rest, &rule.Actions); err != nil {
		return err
	}
	*r = rule
	return nil
}

// When holds a rule's conditions that are not positional.
type When struct {
	// Has matches any of these flags anywhere in the arguments.
	Has []string `json:"has,omitzero"`
	// Cwd is a glob on the working directory.
	Cwd *string `json:"cwd,omitzero"`
	// Env matches environment variables.
	Env map[string]string `json:"env,omitzero"`
	// Under is a command pattern that an ancestor process matches.
	Under *string `json:"under,omitzero"`
	// Tty matches whether the command has a terminal.
	Tty *bool `json:"tty,omitzero"`
	// Captures are globs on the arguments that `{name}` tokens captured.
	Captures map[string]string `json:"captures,omitzero"`
}

// Actions are the actions of a command rule or preset.
type Actions struct {
	// To is `actor`, an executor name, or `executor` together with Executor.
	To *string `json:"to,omitzero"`
	// Executor names the executor when To is `executor`.
	Executor *string `json:"executor,omitzero"`
	// Deny refuses the command with this message.
	Deny *string `json:"deny,omitzero"`
	// Timeout ends the command after this long.
	Timeout *Duration `json:"timeout,omitzero"`
	// Cwd is the command's working directory.
	Cwd *string `json:"cwd,omitzero"`
	// Args adds arguments.
	Args *ArgOps `json:"args,omitzero"`
	// Wrap holds words around the whole command, outermost first.
	Wrap *Words `json:"wrap,omitzero"`
	// Exec replaces the command; may use captures.
	Exec *Words `json:"exec,omitzero"`
	// Env changes the command's environment.
	Env *EnvOps `json:"env,omitzero"`
	// Files are file mappings added to the run's for this command.
	Files []string `json:"files,omitzero"`
	// Use names presets whose actions apply as if written here.
	Use []string `json:"use,omitzero"`
	// Final stops evaluation after this rule.
	Final *bool `json:"final,omitzero"`
}

// ArgOps add arguments to a command.
type ArgOps struct {
	// Prepend goes after the command name, before the user's arguments.
	Prepend *Words `json:"prepend,omitzero"`
	// Append goes after the user's arguments.
	Append *Words `json:"append,omitzero"`
}

// EnvOps change a command's environment.
type EnvOps struct {
	// Set sets variables.
	Set map[string]string `json:"set,omitzero"`
	// Default sets variables only when unset.
	Default map[string]string `json:"default,omitzero"`
	// Prepend prepends to list variables such as `PATH`, joined with `:`.
	Prepend map[string]string `json:"prepend,omitzero"`
	// Append appends to list variables, joined with `:`.
	Append map[string]string `json:"append,omitzero"`
	// Unset removes variables.
	Unset []string `json:"unset,omitzero"`
	// Clear starts from Pass only, instead of the inherited environment.
	Clear *bool `json:"clear,omitzero"`
	// Pass lists the variables Clear keeps.
	Pass []string `json:"pass,omitzero"`
}

// Resolver is a user launcher resolver.
type Resolver struct {
	// Match is the launcher's command pattern.
	Match string `json:"match"`
	// Effective is the command to match again after rewriting.
	Effective *string `json:"effective,omitzero"`
	// Identity is early identity from arguments.
	Identity map[string]string `json:"identity,omitzero"`
	// Sticky makes children inherit the placement.
	Sticky *bool `json:"sticky,omitzero"`
}

// UnmarshalJSON reads a resolver, whose `match` is required.
func (r *Resolver) UnmarshalJSON(data []byte) error {
	type written struct {
		Match     *string           `json:"match"`
		Effective *string           `json:"effective"`
		Identity  map[string]string `json:"identity"`
		Sticky    *bool             `json:"sticky"`
	}
	if isJSONNull(data) {
		return errors.New("invalid type: null, expected a resolver")
	}
	var w written
	if err := strictUnmarshal(data, &w); err != nil {
		return err
	}
	if w.Match == nil {
		return errors.New("missing field `match`")
	}
	*r = Resolver{
		Match:     *w.Match,
		Effective: w.Effective,
		Identity:  w.Identity,
		Sticky:    w.Sticky,
	}
	return nil
}

// UnknownPolicy places launchers whose effective command cannot be known
// statically: `sticky`, `actor`, or an executor's name.
type UnknownPolicy string

// The named unknown-launcher policies.
const (
	// UnknownSticky keeps the launcher's placement.
	UnknownSticky UnknownPolicy = "sticky"
	// UnknownActor runs the command in the actor.
	UnknownActor UnknownPolicy = "actor"
)
