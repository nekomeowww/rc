package humantime

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	cases := map[string]Duration{
		"0":         {},
		"500ms":     {Nanoseconds: 500_000_000},
		"2s":        {Seconds: 2},
		"1m 30s":    {Seconds: 90},
		"3d4h":      {Seconds: 3*86400 + 4*3600},
		"1.5h":      {Seconds: 5400},
		"1.5s":      {Seconds: 1, Nanoseconds: 500_000_000},
		"2µs":       {Nanoseconds: 2000},
		"1h0m0.5s":  {Seconds: 3600, Nanoseconds: 500_000_000},
		"1 0s":      {Seconds: 10},
		"1week 1ms": {Seconds: 7 * 86400, Nanoseconds: 1_000_000},
		"1M":        {Seconds: 2_630_016},
		"1000years": {Seconds: 31_557_600_000},
		"999ms 1ms": {Seconds: 1},
	}
	for text, want := range cases {
		got, err := Parse(text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, text := range []string{
		"",
		" ",
		"s",
		"2",
		"-1s",
		"1x",
		"1.s",
		"1.5ns",
		"0x1p3",
	} {
		_, err := Parse(text)
		assert.Error(t, err, text)
	}
	_, err := Parse("18446744073709551616s")
	assert.ErrorIs(t, err, ErrOverflow)
}

func TestStd(t *testing.T) {
	got, err := Duration{Seconds: 90, Nanoseconds: 5}.Std()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second+5, got)

	maxSeconds := uint64(math.MaxInt64 / int64(time.Second))
	got, err = Duration{Seconds: maxSeconds, Nanoseconds: 854_775_807}.Std()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(math.MaxInt64), got)

	_, err = Duration{Seconds: maxSeconds, Nanoseconds: 854_775_808}.Std()
	assert.ErrorIs(t, err, ErrOverflow)
	_, err = Duration{Seconds: 31_557_600_000}.Std()
	assert.ErrorIs(t, err, ErrOverflow)
}
