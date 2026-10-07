package command

import (
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleFlags(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name     string
		flags    []string
		idle     time.Duration
		deletion time.Duration
	}{
		{name: "omitted clocks are disabled"},
		{name: "explicit zero is disabled", flags: []string{"--idle-timeout=0", "--delete-after-suspended=0"}},
		{name: "opt in", flags: []string{"--idle-timeout=1h", "--delete-after-suspended=24h"}, idle: time.Hour, deletion: 24 * time.Hour},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			flags := pflag.NewFlagSet("lifecycle", pflag.ContinueOnError)
			options := new(LifecycleOptions)
			options.AddFlags(flags)
			require.NoError(t, flags.Parse(scenario.flags))
			require.NoError(t, options.Validate())
			assert.Equal(t, scenario.idle, options.IdleTimeout)
			assert.Equal(t, scenario.deletion, options.DeleteAfterSuspended)
		})
	}
}

func TestLifecycleFlagsRejectNegativeDurations(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"idle-timeout", "delete-after-suspended"} {
		t.Run(flag, func(t *testing.T) {
			flags := pflag.NewFlagSet("lifecycle", pflag.ContinueOnError)
			options := new(LifecycleOptions)
			options.AddFlags(flags)
			require.NoError(t, flags.Parse([]string{"--" + flag + "=-1h"}))
			require.EqualError(t, options.Validate(), "lifecycle durations must not be negative")
		})
	}
}
