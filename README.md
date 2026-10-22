gridwell-plugins

## The ledger

`boundary/ratchet.txt` is this repository's scoreboard, the same shape as
gridwell's `test/boundary/ratchet.txt` and under the same rule, gridwell's
`CLAUDE.md`, How to work 10: one row per measure, each counted by code, and
`make check` fails when a count rises or falls without the new value
recorded. A unit of simplification work is one row, lowered by the mechanism
the row names.

- `lines` and `packages` are ceilings over the production tree.
- The conformance suite (`boundary/conformance_test.go`) spawns each shipped
  binary against a fake of its source and checks the rules of gridwell's
  `docs/plugin-standard.md` that can be checked from outside; any finding
  fails the build. Run `go test -run TestConformance -v ./boundary` (after
  `make build`) to see each finding by plugin and rule.
- Every clock the tree starts is listed in `boundary/allowed-timers.txt`
  with its class; a new one fails the build until it is listed.
