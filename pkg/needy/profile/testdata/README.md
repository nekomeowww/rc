# Profile fixtures

Copied unchanged from [needy](https://github.com/needy-dev/needy) at tag
`v0.0.20`, keeping their relative paths so `extends` and recipe references
resolve alike:

- `builtin/profiles/`: `crates/config/src/builtin/profiles/`.
- `examples/`: every YAML file under `examples/`. `*.recipe.yaml` files are
  recipes that profiles reference, not profiles; the tests skip them.
- `tests/`: profiles embedded in `crates/config/src/profile/mod_test.rs`
  (`PROFILE`, the `extends` and null-removal pairs, and the container run
  options).

`synthetic/forms.yaml` is not from needy: it covers the written forms the
other fixtures do not use, such as string-or-object values, numbers where
strings are allowed, explicit nulls, and scalars that only YAML 1.2 reads as
strings.

`TestNeedyReadsReencodedProfilesTheSame` compares what the needy binary
named by `NEEDY_BIN` reads from each fixture and from its re-encoded form.
