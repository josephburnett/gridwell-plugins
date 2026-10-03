package memo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// File is one snapshot of a plugin's memory in the state_dir the node hands
// it, under cache.db's contract: disposable, safe to delete, rewarmed by use.
// A file that is missing, unreadable, corrupt or of another version is a cold
// start, never a failure; one that cannot be written costs the next restart a
// walk, never the answer.
type File[T any] struct {
	path    string
	version int
	logf    func(format string, args ...any)

	mu      sync.Mutex
	failing bool // a save failed and none has landed since: logged once (rule 14)
}

// envelope is the file's shape. data is required, so a file from before
// memo, which carries a version and no data, is not read as an empty memory.
type envelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// NewFile names the cache file name inside stateDir. version starts at 1 and
// is bumped whenever T changes shape, so an older file is refused rather than
// misread. An empty stateDir is a node that hands none: the result is nil,
// whose Load is always cold and whose Save writes nothing. logf nil is the
// standard logger.
func NewFile[T any](stateDir, name string, version int, logf func(format string, args ...any)) *File[T] {
	if stateDir == "" {
		return nil
	}
	if version < 1 {
		panic(fmt.Sprintf("memo: cache file %s: version %d, want 1 or more", name, version))
	}
	if logf == nil {
		logf = log.Printf
	}
	return &File[T]{path: filepath.Join(stateDir, name), version: version, logf: logf}
}

// Path is where the file lives, "" for the nil File.
func (f *File[T]) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// Load reads the last snapshot. ok is false on a cold start. A file that
// exists and cannot be used is logged, because a cache must not vanish in
// silence, and is left for the next Save to replace.
func (f *File[T]) Load() (v T, ok bool) {
	if f == nil {
		return v, false
	}
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return v, false
	}
	if err == nil {
		v, err = f.decode(raw)
	}
	if err != nil {
		f.logf("memo: cache %s: %v (starting cold)", f.path, err)
		var zero T
		return zero, false
	}
	return v, true
}

func (f *File[T]) decode(raw []byte) (v T, err error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return v, err
	}
	if env.Version != f.version {
		return v, fmt.Errorf("version %d, want %d", env.Version, f.version)
	}
	if len(env.Data) == 0 {
		return v, errors.New("no data")
	}
	err = json.Unmarshal(env.Data, &v)
	return v, err
}

// Save writes the snapshot snap takes, atomically: the next boot sees the
// whole old file or the whole new one. snap runs under the file's lock, so of
// two racing saves the one that writes last holds the later memory. The
// directory is minted if missing. A failure is returned, and logged once
// until a save lands.
func (f *File[T]) Save(snap func() T) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.write(snap())
	switch {
	case err != nil && !f.failing:
		f.failing = true
		f.logf("memo: cache %s: %v", f.path, err)
	case err == nil:
		f.failing = false
	}
	return err
}

func (f *File[T]) write(v T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(envelope{Version: f.version, Data: data})
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(f.path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has moved it
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}
