# gridwell-plugin-proc

A projection of the host's process tree into Gridwell: each process is a
well whose grid lists its direct children, plus an `@info` tile describing
it. It reads `/proc`, so it serves on Linux only. On a host without `/proc`
(macOS) it builds, and refuses with the reason on the plugin's row.

## Setup

```yaml
plugins:
  - kind: proc
    label: shells
    config:
      pid: 4242
```

`pid` is the process whose tree the plugin serves; no `pid` means pid 1, the
whole table. A `pid` that is not a positive integer is refused. One that is
not running, or a `/proc` that cannot be read, is refused with that reason
until it is there; once served, a pid that exits leaves the plugin dark, not
broken.

## Keys

- A process's key is its pid (`4242`), and its grid's context is the same
  pid. A pid the kernel reuses is a new process under the old key.
- `info:<pid>` is the `@info` tile in that pid's own grid: name, state,
  memory, threads, cwd and command line, as markdown.
- A listing is not authoritative: a child unreadable this pass is not gone.
  The node asks `Probe`, which answers for the grid it names, so a child
  reparented after its parent exits leaves the old grid.

## Delete

Deleting a process's tile sends it SIGTERM. The tile goes once the process
is gone, at once for one already gone. A process belonging to another user
is refused with that reason. Deleting `@info` is refused: it describes a
process and is not one.

## Status

A tile's status carries one emoji when the process is worth noticing, and
nothing otherwise. The label is always the pid.

- 💀 a zombie: it has exited and its parent has not reaped it.
- ⏸ stopped, by a signal or a tracer.

## Changes

`/proc` cannot announce a change: inotify sees nothing there, and the netlink
process connector needs `CAP_NET_ADMIN`. So the plugin polls, and only what
is shown. While some client shows a pid's grid (the node's
`WatchRequest.contexts`), the plugin reads that pid's children every 2 s
(`PollEvery`), and when the set or a child's status emoji differs from the
last read it sends one `ContextChanged` for the pid. A poll stops 10 s after
the last client stops showing its pid (`memo.DefaultLinger`). Nothing shown,
nothing read. The same poll reads the pid's `@info` body, and when it differs
from the last read it sends one `EntryChanged` with the `@info` entry, so an
open `@info` shows the process as it is now. Its `content_stamp` is a hash
of the body: the process table keeps no version and the plugin no memory.
