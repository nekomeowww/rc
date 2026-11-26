package profile

import (
	"encoding/json"
	"errors"
	"strings"
)

// RunnerSelector is which runner an actor or an executor runs on: a
// runner's name, `{ labels: [KEY, KEY=VALUE] }`, or `{ client: true }`.
// Exactly one of its fields is set.
type RunnerSelector struct {
	// Name is a runner's name.
	Name string
	// Labels select the one connected runner that carries every label.
	Labels []string
	// Client selects the execution capability supplied with the run's
	// creation.
	Client bool
}

var errRunnerSelector = errors.New(
	"a runner selector is a runner's name, { labels: [KEY, KEY=VALUE, ...] }, or { client: true }",
)

type runnerLabels struct {
	Labels []string `json:"labels"`
}

type runnerClient struct {
	Client bool `json:"client"`
}

// MarshalJSON writes the selector in its written form.
func (r RunnerSelector) MarshalJSON() ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	switch {
	case r.Client:
		return json.Marshal(runnerClient{Client: true})
	case r.Labels != nil:
		return json.Marshal(runnerLabels{Labels: r.Labels})
	}
	return json.Marshal(r.Name)
}

// UnmarshalJSON reads a runner's name, `{ labels }` or `{ client: true }`.
func (r *RunnerSelector) UnmarshalJSON(data []byte) error {
	var selector RunnerSelector
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		if err := json.Unmarshal(data, &selector.Name); err != nil {
			return err
		}
	case '{':
		var labels runnerLabels
		var client runnerClient
		switch {
		case strictUnmarshal(data, &labels) == nil && labels.Labels != nil:
			selector.Labels = labels.Labels
		case strictUnmarshal(data, &client) == nil && hasKey(data, "client"):
			if !client.Client {
				return errors.New("client selection requires { client: true }")
			}
			selector.Client = true
		default:
			return errRunnerSelector
		}
	default:
		return errRunnerSelector
	}
	if err := selector.check(); err != nil {
		return err
	}
	*r = selector
	return nil
}

func (r RunnerSelector) check() error {
	switch {
	case r.Client:
		if r.Name != "" || r.Labels != nil {
			return errRunnerSelector
		}
	case r.Labels != nil:
		if r.Name != "" {
			return errRunnerSelector
		}
		if len(r.Labels) == 0 {
			return errors.New("`labels` selects no runner when empty; name at least one label")
		}
		for _, label := range r.Labels {
			key, _, _ := strings.Cut(label, "=")
			if key == "" {
				return errors.New("a label is KEY or KEY=VALUE, not " + label)
			}
		}
	case r.Name == "":
		return errors.New("a runner's name is empty")
	}
	return nil
}

// hasKey reports whether a JSON object has key.
func hasKey(data []byte, key string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	_, ok := object[key]
	return ok
}
