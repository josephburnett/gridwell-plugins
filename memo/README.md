# memo

A plugin's memory of its source, written once for every plugin that keeps
one (`docs/plugin-standard.md` rule 15 in the gridwell repository). It owns
four things.

| Type | Owns | Rules |
|---|---|---|
| `File[T]` | the cache file in `state_dir` | `cache.db`'s contract, 14 |
| `Flights` | one shared walk per key, and how a read answers | 7, 13, 14 |
| `Changes` | the `Watch` fan-out, and when background work runs | 8, 9, 18 |
| `Life` | the context walks and work run under | 13 |

## File

`NewFile[T](stateDir, name, version, logf)` names a JSON snapshot of `T`.
`Load` answers it, or a cold start when the file is missing, corrupt, of
another version, or written before memo (logged, except a missing file).
`Save(func() T)` writes atomically, private (0600), minting the directory;
the snapshot is taken under the file's lock so the last write holds the
latest memory. A failing save logs once until one lands. With no `state_dir`
the File is nil and both are no-ops.

## Flights

`NewFlights(life, FlightOptions{Walk, Window, ...})`.

- `Read(ctx, key, warm)` is a call's read. Fresh memory answers as it is.
  Otherwise a walk starts, or the one in flight is joined. A warm read never
  waits. A cold read waits at most `FirstAnswer`, then answers memory so far.
  The `unreachable` it returns is the last refresh's failure, for
  `ListResponse.unreachable`; the read itself does not fail on it.
- `Rewalk(key)` walks whatever the freshness, once more if a walk is in
  flight.
- `Outcome(key, err)` records a refresh that was not a walk (a glance, a
  feed ending).
- `Covers` lets a broad walk answer narrow keys (gitlab's root and weeks).
  `Fresh` replaces the clock window for a source that tells (hey's feed).
- `Landed` runs before waiting readers are released: save the File and
  publish the change there.
- `WalkedAt` and `Restore` carry the freshness stamps through the File. A
  stamp from the future is not fresh.
- A key's failure is logged when it starts and again only after a landing
  clears it.

## Changes

`NewChanges(life, ChangeOptions{Work, Do, Unscoped, ...})`.

- `Serve(req.Contexts, stream)` is the plugin's whole `Watch`. It starts the
  scope's work, sends the header, and sends one `ContextChanged` per queued
  context, then one `EntryChanged` per queued entry.
- `Publish(contexts...)` reaches every stream without waiting on any. A
  context queued twice is sent once. A stream owed more than `Buffer`
  contexts is sent its whole scope instead.
- `PublishEntry(context, entry)` says one entry changed in place (rule 18):
  the entry as `List` answers it now, its `content_stamp` moved with its
  bytes. An entry queued twice is sent once, as published last, and no
  overflow drops one.
- `Work` maps a context in scope to units of background work, and `Do` runs
  each unit while some stream needs it, ending `Linger` after the last
  leaves. This is the only place background work starts. `Poll` is the loop
  for a source that cannot tell. `Watching(unit)` reports whether a unit runs.

## Life

`NewLife()` in `FromConfig`. Walks and work run under `life.Context()`, never
`context.Background()`. A test calls `End` to stop them.

## calendar

`memo/calendar` is the one placement rule for things with a creation time
(`docs/plugin-standard.md` rule 10): the hint is a function of the time
alone, never of list position.

- `Cell(created, w)`: x is the local day since `Epoch` times `w`, newest to
  the right; y is the local hour, 0 to 23. Things from the same hour share a
  cell and the node stacks them. `w` is the tile's width, so neighbouring
  days never overlap.
- `WeekCell(monday)`: one row per month, newest at the top, weeks left to
  right, for a calendar of weeks.
- `Epoch` is 2026-08-24, fixed forever: moving it moves every untouched tile.

Local time, because a calendar means the user's day. A tile the user has
touched keeps its place whatever the zone.

## Adopting it

1. Require `github.com/josephburnett/gridwell-plugins/memo` (tagged
   `memo/vX.Y.Z`; the workspace resolves it locally).
2. Replace the package's `LoadCache`/`SaveCache` with a `File` of its
   snapshot type, and carry `Flights.WalkedAt` inside that snapshot. An old
   cache file is a cold start, once.
3. Replace the plugin's `flight`, `flights`, `failed`, `walkedAt` and `again`
   state with one `Flights`. `List` passes `warm` and puts the returned
   reason in `ListResponse.unreachable`.
4. Replace the plugin's watch file with `Changes`. `Watch` becomes
   `return p.changes.Serve(req.GetContexts(), stream)`.
5. Delete `Run` and the `go p.Run(...)` in `FromConfig`. The refresher, glance
   or feed becomes a `Do`, and `Work` names which contexts need it.
6. Keep one test that the plugin's memory survives a restart through `File`.
