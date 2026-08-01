# `pkg/`

Reserved for types CloudSDD exposes to code outside this repository.

It is empty, and it is tracked only because git does not carry empty
directories: without a file here, the layout CLAUDE.md's Task 0 mandates
would be absent from a fresh clone and every reader would have to be told
that `pkg/` is intentional rather than missing.

Nothing has moved here yet, deliberately. Everything under `internal/` is
free to change shape between RFCs, and a type promoted to `pkg/` stops
being free. The candidates when that changes are the Specification types
in `internal/spec` and the `CloudProvider` interface in
`internal/provider` — the two things an external caller would need to
build a Specification or implement a new cloud. Promoting either is a
public API commitment and needs its own RFC.
