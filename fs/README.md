# gridwell-plugin-fs

A projection of one directory tree into Gridwell: each directory is a grid,
each subdirectory a well, each file a tile. Deleting a tile moves the file to
the trash.

## Setup

```yaml
plugins:
  - kind: fs
    label: docs
    config:
      root: /home/me/docs
```

No `root` means the plugin declares no collection and contributes nothing to
the (+) menu. A `root` that does not exist, is not a directory, or cannot be
read is refused with that reason, and the plugin's row shows it broken until
the directory is there.

## Changes

The plugin watches only the directories some client shows right now: the
node's `WatchRequest.contexts`. A file open in a pane is covered by its
directory. A directory that leaves the scope loses its watch at once, and
while nothing is shown nothing is watched, so a large tree never runs into
the operating system's watch limit. A node from before scopes asks with none,
and then nothing is watched.

Watches are the OS's own change notifications through
[fsnotify](https://github.com/fsnotify/fsnotify): inotify on Linux, kqueue on
macOS and the BSDs, ReadDirectoryChangesW on Windows. kqueue holds an open
file for every file in a watched directory, so on macOS the limit is the
open-file limit.

- Changes in one directory within 200 ms (`DebounceWindow`) are one
  `ContextChanged` for that directory: an editor's save or a build is many
  events.
- A file created, written, renamed or removed announces its directory. The
  plugin never sends `EntryRemoved`: the node's next listing retires the key.
- A shown directory that stops existing announces itself and its parent (if
  shown) and drops its watch. One created again at a shown path is watched
  again.
- Running out of watches is the stream's error, `ResourceExhausted`, logged
  once: the node shows it as the source's health until a scope fits. Raise
  `fs.inotify.max_user_watches` (Linux) or the open-file limit (macOS), or
  show fewer directories.
