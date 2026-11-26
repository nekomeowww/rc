package profile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtures lists the profile fixtures under testdata, relative to it.
// Recipes beside them are support files, not profiles.
func fixtures(t *testing.T) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir("testdata", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".recipe.yaml") {
			return nil
		}
		rel, err := filepath.Rel("testdata", path)
		if err != nil {
			return err
		}
		names = append(names, rel)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, names)
	return names
}

func TestRoundTripFixtures(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			require.NoError(t, err)

			first, err := Decode(data)
			require.NoError(t, err)
			encoded, err := first.Encode()
			require.NoError(t, err)
			second, err := Decode(encoded)
			require.NoError(t, err, "re-encoded profile:\n%s", encoded)
			assert.Equal(t, first, second)

			again, err := second.Encode()
			require.NoError(t, err)
			assert.Equal(t, string(encoded), string(again))
		})
	}
}

const actorOnly = "actor: { driver: macos-vz }\n"

func TestDecodeRefusesUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"top level":         actorOnly + "bogus: 1\n",
		"top level null":    actorOnly + "bogus: null\n",
		"actor":             "actor: { driver: macos-vz, cwd: /tmp }\n",
		"actor workspace":   "actor: { driver: macos-vz, workspace: { path: ~/w, mount: x } }\n",
		"executor":          actorOnly + "executors: { x: { remote: host } }\n",
		"executor runtime":  actorOnly + "executors: { x: { runtime: remote } }\n",
		"executor docker":   actorOnly + "executors: { x: { container: { args: [-it] } } }\n",
		"executor null key": actorOnly + "executors: { x: { bogus: ~ } }\n",
		"mount": actorOnly +
			"executors: { x: { container: { mounts: [{ type: tmpfs, target: /t, read_only: true }] } } }\n",
		"ulimit":       actorOnly + "executors: { x: { container: { ulimits: { n: { soft: 1, hard: 2, x: 3 } } } } }\n",
		"rule action":  actorOnly + "commands: [{ match: x, bogus: 1 }]\n",
		"rule when":    actorOnly + "commands: [{ match: x, when: { script: y } }]\n",
		"file mapping": actorOnly + "files: { a: { mode: mount, from: 'host:a', to: 'actor:b', bogus: 1 } }\n",
		"on-conflict": actorOnly +
			"files: { a: { mode: sync, from: 'host:a', to: 'actor:b', on-conflict: { x: { driver: d, y: 1 } } } }\n",
		"fault":            actorOnly + "chaos: { faults: { a: { exec: x, exit: 1, bogus: 1 } } }\n",
		"image definition": "actor: { driver: macos-vz, image: { layers: [], bogus: 1 } }\n",
		"layer":            "actor: { driver: macos-vz, image: { layers: [{ run: x, bogus: 1 }] } }\n",
		"merge key":        "actor: { <<: { driver: macos-vz } }\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(text))
			require.Error(t, err)
		})
	}
}

func TestDecodeRefusesWrongTypes(t *testing.T) {
	cases := map[string]string{
		"string for map":        "actor: { driver: macos-vz, env: x }\n",
		"number for string":     "actor: { driver: macos-vz, env: { A: 1 } }\n",
		"yes for bool":          "actor: { driver: macos-vz, limits: { single_process: yes } }\n",
		"unknown driver":        "actor: { driver: linux-native }\n",
		"negative uid":          "user: { uid: -1 }\nactor: { driver: macos-vz }\n",
		"uid overflow":          "user: { uid: 4294967296 }\nactor: { driver: macos-vz }\n",
		"float seed":            "actor: { driver: macos-vz, seed: 2.5 }\n",
		"bad duration":          actorOnly + "executors: { x: { idle_timeout: 3 fortnights } }\n",
		"negative duration":     actorOnly + "executors: { x: { idle_timeout: -1 } }\n",
		"float size":            actorOnly + "executors: { x: { container: { memory: 2.5 } } }\n",
		"string cpus":           actorOnly + "executors: { x: { container: { cpus: '2' } } }\n",
		"gpu count list":        actorOnly + "executors: { x: { container: { gpus: [0] } } }\n",
		"bad dns":               actorOnly + "executors: { x: { container: { dns: [foo] } } }\n",
		"on_full new":           actorOnly + "executors: { x: { on_full: new } }\n",
		"runner number":         "actor: { driver: macos-vz, runner: 5 }\n",
		"runner client false":   "actor: { driver: macos-vz, runner: { client: false } }\n",
		"runner empty labels":   "actor: { driver: macos-vz, runner: { labels: [] } }\n",
		"builtin image":         "actor: { driver: macos-vz, image: 'builtin:codex' }\n",
		"float feature option":  "actor: { driver: macos-vz, image: { layers: [{ feature: f, with: { v: 2.5 } }] } }\n",
		"run and feature layer": "actor: { driver: macos-vz, image: { layers: [{ run: x, feature: y }] } }\n",
		"empty hook argv":       "actor: { driver: macos-vz, image: { hooks: { start: [[]] } } }\n",
		"side without prefix":   actorOnly + "files: { a: { mode: mount, from: x, to: 'actor:y' } }\n",
		"null word":             "actor: { driver: macos-vz, command: [a, null] }\n",
		"unknown conflict": actorOnly +
			"files: { a: { mode: sync, from: 'host:a', to: 'actor:b', on-conflict: { x: ours } } }\n",
		"fault without effect":   actorOnly + "chaos: { faults: { a: { exec: 'curl **' } } }\n",
		"fault of two domains":   actorOnly + "chaos: { faults: { a: { exec: x, file: { open: y }, exit: 1 } } }\n",
		"status out of range":    actorOnly + "chaos: { faults: { a: { net: { request: x }, status: 70000 } } }\n",
		"missing actor":          "user: { name: agent }\n",
		"missing driver":         "actor: { command: [sh] }\n",
		"rule without match":     actorOnly + "commands: [{ to: actor }]\n",
		"not a mapping":          "- actor\n",
		"duplicate key":          actorOnly + "actor: { driver: macos-vz }\n",
		"extends number":         "extends: 1\n",
		"infinite float":         actorOnly + "chaos: { faults: { a: { exec: x, exit: 1, probability: .inf } } }\n",
		"two documents":          actorOnly + "---\nactor: { driver: macos-vz }\n",
		"integer mapping key":    "actor: { driver: macos-vz, env: { 1: a } }\n",
		"unsupported custom tag": "actor: { driver: !custom macos-vz }\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(text))
			require.Error(t, err)
		})
	}
}

func TestDecodeChecksRequiredKeysOnlyWithoutExtends(t *testing.T) {
	_, err := Decode([]byte("extends: base.yaml\nactor: { command: [sh] }\nfiles: { a: { writable: true } }\n"))
	require.NoError(t, err)

	_, err = Decode([]byte(actorOnly + "files: { a: { writable: true } }\n"))
	require.Error(t, err)
}

func TestEncodeKeepsPresenceAndForms(t *testing.T) {
	p, err := Decode([]byte(`extends: parent.yaml
actor:
  command: echo hi
  limits: { single_process: false }
  env: { A: "1" }
executors:
  build: { max_procs: 0, idle_timeout: 90, unavailable_timeout: 1m 30s }
run_files: []
files:
  pi-in: null
  keep: { writable: false }
trace: { sinks: [] }
`))
	require.NoError(t, err)
	require.NotNil(t, p.Actor.Limits.SingleProcess)
	assert.False(t, *p.Actor.Limits.SingleProcess)
	assert.Nil(t, p.Actor.Limits.SelfReplace)
	assert.Nil(t, p.User)
	assert.NotNil(t, p.RunFiles)
	assert.Empty(t, p.RunFiles)
	assert.NotContains(t, p.Files.Mappings, "pi-in")
	assert.Equal(t, []Path{{"files", "pi-in"}}, p.Nulls)
	assert.True(t, p.Actor.Command.IsString())
	assert.Equal(t, []string{"echo", "hi"}, p.Actor.Command.Values())

	encoded, err := p.Encode()
	require.NoError(t, err)
	text := string(encoded)
	for _, want := range []string{
		"command: echo hi",
		"single_process: false",
		"max_procs: 0",
		"idle_timeout: 90",
		"unavailable_timeout: 1m 30s",
		"run_files: []",
		"pi-in: null",
		"writable: false",
		"sinks: []",
	} {
		assert.Contains(t, text, want)
	}
	for _, absent := range []string{"self_replace", "user:", "driver:", "key:", "on_full"} {
		assert.NotContains(t, text, absent)
	}
}

func TestOnConflictKeepsItsOrder(t *testing.T) {
	p, err := Decode([]byte(`actor: { driver: macos-vz }
files:
  s:
    mode: sync
    from: "host:s"
    to: "actor:~/s"
    on-conflict: { z: run, a: { driver: " tool " }, m: lines }
`))
	require.NoError(t, err)
	rules := p.Files.Mappings["s"].OnConflict
	require.Len(t, rules, 3)
	assert.Equal(t, "z", rules[0].Pattern)
	assert.Equal(t, "a", rules[1].Pattern)
	assert.Equal(t, "tool", rules[1].Choice.Command())
	assert.Equal(t, "m", rules[2].Pattern)

	encoded, err := p.Encode()
	require.NoError(t, err)
	text := string(encoded)
	assert.Less(t, strings.Index(text, "z: run"), strings.Index(text, "a:"))
	assert.Less(t, strings.Index(text, "a:"), strings.Index(text, "m: lines"))
}

func TestEncodeQuotesStringsNeedyWouldReadAsOtherTypes(t *testing.T) {
	p, err := Decode([]byte(`actor:
  driver: macos-vz
  env: { A: "1", B: "true", C: "~", D: "", E: "1.5", F: "0x10", G: "null", H: 007, I: "-1" }
  command: ["1", "2.0", "false"]
`))
	require.NoError(t, err)
	encoded, err := p.Encode()
	require.NoError(t, err)
	again, err := Decode(encoded)
	require.NoError(t, err, "re-encoded profile:\n%s", encoded)
	assert.Equal(t, p, again)
	assert.Equal(t, "007", again.Actor.Env["H"])
}

func TestEditAndEncode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "examples", "smoke", "profile.yaml"))
	require.NoError(t, err)
	p, err := Decode(data)
	require.NoError(t, err)

	maxProcs := uint32(16)
	build := p.Executors["build"]
	build.MaxProcs = &maxProcs
	p.Executors["build"] = build
	words := NewWords("sleep", "infinity")
	p.Actor.Command = &words

	encoded, err := p.Encode()
	require.NoError(t, err)
	again, err := Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, uint32(16), *again.Executors["build"].MaxProcs)
	assert.Equal(t, []string{"sleep", "infinity"}, again.Actor.Command.Values())
}

func TestWordsTakeNumbersAndBooleansAsText(t *testing.T) {
	p, err := Decode([]byte("actor: { driver: macos-vz, command: [sleep, 1.50, true, 0x10, 1e3, -2] }\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"sleep", "1.5", "true", "16", "1000", "-2"}, p.Actor.Command.Values())
}

func TestDurations(t *testing.T) {
	cases := map[string]time.Duration{
		"500ms":     500 * time.Millisecond,
		"2s":        2 * time.Second,
		"1m 30s":    90 * time.Second,
		"3d4h":      76 * time.Hour,
		"1.5h":      90 * time.Minute,
		"0":         0,
		"2.5":       2500 * time.Millisecond,
		" 10m ":     10 * time.Minute,
		"2µs":       2 * time.Microsecond,
		"1h0m0.5s":  time.Hour + 500*time.Millisecond,
		"1 0s":      10 * time.Second,
		"1week 1ms": 7*24*time.Hour + time.Millisecond,
	}
	for text, want := range cases {
		d, err := ParseDuration(text)
		require.NoError(t, err, text)
		got, err := d.Duration()
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
	for _, text := range []string{"", "s", "-1s", "1x", "1.s", "inf", "NaN", "1.5ns", "0x1p3"} {
		_, err := ParseDuration(text)
		assert.Error(t, err, text)
	}
	assert.Equal(t, "1m30s", NewDuration(90*time.Second).String())
}

func TestByteSizes(t *testing.T) {
	cases := map[string]uint64{
		"512MiB":  512 << 20,
		"16GiB":   16 << 30,
		"2G":      2 << 30,
		"1.5 TiB": 3 << 39,
		"64":      64 << 20,
		"0.5G":    512 << 20,
	}
	for text, want := range cases {
		s, err := ParseByteSize(text)
		require.NoError(t, err, text)
		got, err := s.Bytes()
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
	for _, text := range []string{"", "1KiB", "MiB", "1..2G", "-1G"} {
		_, err := ParseByteSize(text)
		assert.Error(t, err, text)
	}
	got, err := NewByteSizeMiB(3).Bytes()
	require.NoError(t, err)
	assert.Equal(t, uint64(3<<20), got)
}

func TestPlainScalarsResolveAsNeedyReadsThem(t *testing.T) {
	cases := map[string]string{
		"":           jsonNull,
		"~":          jsonNull,
		"Null":       jsonNull,
		"True":       "true",
		"yes":        `"yes"`,
		"on":         `"on"`,
		"007":        `"007"`,
		"-07":        `"-07"`,
		"0":          "0",
		"+5":         "5",
		"-5":         "-5",
		"0x1F":       "31",
		"-0x10":      "-16",
		"0o17":       "15",
		"0b101":      "5",
		"1_000":      `"1_000"`,
		"1.25":       "1.25",
		"1.":         "1.0",
		".5":         "0.5",
		"1e3":        "1000.0",
		"1e999":      `"1e999"`,
		"nan":        `"nan"`,
		"2001-12-14": `"2001-12-14"`,
		"0777":       `"0777"`,
		"08":         `"08"`,
	}
	for plain, want := range cases {
		got, err := plainScalarJSON(plain)
		require.NoError(t, err, plain)
		assert.Equal(t, want, got, plain)
	}
}
