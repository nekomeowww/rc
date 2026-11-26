package profile

// Trace is what a run's trace records and where it goes.
type Trace struct {
	// Sinks are where events go; empty records nothing, absent means
	// `sqlite` and `jsonl`.
	Sinks []TraceSink `json:"sinks,omitzero"`
	// Timing records per-hook latencies as `trace.timing` events.
	Timing *bool `json:"timing,omitzero"`
}

// TraceSink is where trace events go.
type TraceSink string

// The trace sinks.
const (
	// TraceSinkSqlite is the daemon's `trace.db`.
	TraceSinkSqlite TraceSink = "sqlite"
	// TraceSinkJsonl is `trace.jsonl` in the run's directory.
	TraceSinkJsonl TraceSink = "jsonl"
)

// UnmarshalJSON reads a known trace sink.
func (s *TraceSink) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, s, "trace sink", TraceSinkSqlite, TraceSinkJsonl)
}
