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
read refuses `Info` with that reason, and the plugin's row shows it broken
until the directory is there; no restart is needed. Once the root has been
served, losing it later is not a refusal: the node keeps showing what it
remembers and marks the source dark.

The root may itself be a symlink. Everything under it is spelled from the
directory it resolves to.

## Tiles

- A directory is a well onto its own grid.
- A file a browser presents whole (images, video, audio, HTML, PDF) is a url
  tile whose page the node serves through its `/content/` door. A page's
  relative resources are served from its own directory and below, never
  above it.
- Every other file is a text tile. Plain-text families (source, config,
  logs, data) show verbatim; markdown and org render, with the source one
  toggle away; anything else shows a short summary: name, path, size,
  modified time.
- A text body is at most 4 MiB. A longer file shows its first 4 MiB, in the
  same presentation.

## Editing

A text tile whose body is the file's own bytes, a plain-text or markdown/org
file of at most 4 MiB, is editable, and a save writes the whole file: a
temporary file beside it, renamed over it, so the file is the old bytes or
the new and never half of each, and keeps its mode. A save names the
version of the file it was typed over (its modified time and size); if the
file changed on disk since, the save is refused as a conflict, the tile
shows the file as it is now, and nothing on disk is overwritten.

A save is refused with its reason, and the file left alone, when the tile
shows a summary rather than the bytes, the file is past 4 MiB, the file or
its directory does not let you write, or the key is not in the tree.

Listings are authoritative: a directory read is the whole directory, so a
file that is not listed is gone, and its tile goes with it. A directory that
is gone, or is now a file, lists empty. A directory that cannot be read
right now (permissions, I/O) is not empty: the node keeps showing what it
remembers and marks the source dark.

## Symlinks

A symlink is a link, never a second copy of what it points at, so every real
file and directory has one key.

- A link to a file in the tree is a link tile (drawn dashed) to that file.
  It reads, previews and opens as the file.
- A link to a directory in the tree is a well onto that directory's own
  grid. A link back up the tree is the same grid again, not an endless
  descent.
- A link whose target is outside the root, or does not exist, or loops, is
  a dead link: greyed, labelled, deletable. Nothing outside the root is
  read or served through it. A broken link comes alive when its target
  appears.
- Deleting a link tile trashes the link, never its target.

Tiles placed under a symlinked directory before this version are dead: the
path through the link is no longer a key. The same files are under the
link's target, where they always were.

## Deleting

Delete moves the file or directory to the user's trash, never unlinks it:

- macOS: Finder's trash, `~/.Trash`. A name already there takes the next
  free `name 2.ext`.
- Linux and other systems: the freedesktop.org home trash,
  `$XDG_DATA_HOME/Trash` (default `~/.local/share/Trash`), with the
  `.trashinfo` record a file manager needs to restore it.

Deleting something already gone succeeds.

## Changes

The plugin watches only the directories some client shows right now: the
node's `WatchRequest.contexts`. A file open in a pane is covered by its
directory. A directory that leaves the scope loses its watch at once, and
while nothing is shown nothing is watched, so a large tree never runs into
the operating system's watch limit. A node from before scopes asks with
none, and then nothing is watched. A context that is not a directory of the
tree, such as a dead link's target, is never watched.

Watches are the OS's own change notifications through
[fsnotify](https://github.com/fsnotify/fsnotify): inotify on Linux, kqueue on
macOS and the BSDs, ReadDirectoryChangesW on Windows. kqueue holds an open
file for every file in a watched directory, so on macOS the limit is the
open-file limit.

- Changes in one directory within 200 ms (`DebounceWindow`) are told once:
  an editor's save or a build is many events.
- A file created, removed, renamed or re-stamped (its attributes changed)
  announces its directory as a `ContextChanged`; the node lists it and tells
  clients only if the listing moved, and that listing retires a gone key.
- A file written in place, or created over its own name as an editor's save
  does, is an `EntryChanged` carrying the file's entry re-read, so a pane
  showing the file, and the file's face in its grid, show the new bytes.
- A shown directory that stops existing announces itself and its parent (if
  shown) and drops its watch. One created again at a shown path is watched
  again.
- Running out of watches ends the stream with `ResourceExhausted`, logged
  once. The node shows the source as "live updates off" with that reason:
  grids still list and open, but a change shows only when the grid is next
  read. Raise `fs.inotify.max_user_watches` (Linux) or the open-file limit
  (macOS), or show fewer directories.
