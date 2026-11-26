# needy protos

A copy of `proto/needy/v1` from [needy](https://github.com/needy-dev/needy)
at the release in `VERSION`. Do not edit these files: run
`hack/proto-sync [TAG]` to copy another release and `make proto` to
regenerate `apis/sdk/go/needy/v1`.

needy's protos have no wire-compatibility promise before 1.0, so the copy,
the needy image that the manager deploys, and the runners must come from
one release.
