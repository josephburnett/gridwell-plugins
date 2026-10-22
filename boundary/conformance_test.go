// The conformance suite holds every shipped plugin binary to the rules of
// gridwell's docs/plugin-standard.md that a black-box harness can check: it
// spawns each binary the way the node does (compose.LoadPlugin), against a
// fake of its source that counts what it is asked, and reads only the wire.
// A rule a plugin breaks is a finding, and TestConformance fails on any.
//
// Checked here:
//
//	1   a Watch Info declares is served (it sends its header); one it does
//	    not declare is Unimplemented
//	2   Info refuses a config whose source is missing, with a reason, rather
//	    than answering grids; passes once the source is there, no restart;
//	    and latches, so a source that breaks after the first pass is weather
//	3   once Info has passed, an outage or a refused credential reads
//	    Unavailable or a cache answer naming the reason, never a verdict
//	5   a link_target names a grid and a key, and the key is listed in that
//	    grid as itself, not as another link
//	7   a warm read during an outage answers the cached entries, says why in
//	    unreachable, and does not claim authoritative
//	8   with no Watch held the plugin asks its source nothing, and under a
//	    Watch it does (so the zero is evidence, not a fake nobody calls)
//	9   the header half: a Watch stream sends its header on accept
//	11  a status_detail is one symbol, never a word
//	12  every text entry declares text_presentation plain or both
//	18  no stream sends the retired EntryRemoved
//	19  a plugin that declares writable refuses a write claiming a stale
//	    stamp with FailedPrecondition and keeps the bytes
//	20  Find asks the source nothing; a file tree answers Unimplemented
//
// Not mechanical from outside the binary: 4 (whether a read was definitive is
// the plugin's own knowledge of its source), 6 (a plugin whose keys live in
// one grid answers for the plugin as a whole, by the rule), 9's collapse and
// overflow (need a burst the harness cannot time against the plugin's
// window), 10 (a hint's purity needs two listings that differ in order only,
// which the fakes cannot stage for every source), 13 (a source call's context
// is inside the process), 14 (log lines are prose, and an episode's edges
// are the plugin's), 15 (which package the cache code lives in is a source
// fact, not a wire one), 16 (the seam tests live in gridwell), 17 (a README
// read against the code) and 18's EntryChanged-equals-List.
package boundary

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell/api/compose"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// callTimeout bounds one call; a cold read waits on its walk for at most the
// plugin's first-answer bound, a few seconds.
const callTimeout = 10 * time.Second

// idleWindow is how long a plugin with no Watch is watched for work. Every
// subject's refresh is configured well under it, so a poll that ran without
// a stream would be seen several times.
const idleWindow = 3 * time.Second

// meter reads how much a plugin has asked of its source. A counter only
// rises; a level (fs's change watches) is what is held now.
type meter struct {
	read  func(pid int) int64
	level bool
	// slack is what a counter may rise by with no work: the process's own
	// reads of what is not its source.
	slack int64
}

func (m meter) quiet(before, after int64) bool {
	if m.level {
		return after == 0
	}
	return after-before <= m.slack
}

func (m meter) busy(before, after int64) bool {
	if m.level {
		return after > 0
	}
	return after-before > m.slack
}

// source is one running fake of a plugin's source.
type source struct {
	cfg   map[string]string
	meter *meter // nil: the plugin has no source to ask
	// outage stages a transport-shaped failure; revoke a refused credential.
	// Either is nil where the source has no such state.
	outage, revoke func(on bool)
	// breakIt makes the source unservable after Info passed, for the latch.
	breakIt func()
	close   func()
}

// subject is one shipped plugin and how to stand its source up.
type subject struct {
	kind string
	// unixProc: the plugin reads /proc, and is checked on Linux only.
	unixProc bool
	// caches: the plugin keeps a cache of its source (rule 7).
	caches bool
	// fileTree: the source is a file tree, which finds nothing (rule 20).
	fileTree bool
	start    func(dir string) (*source, error)
	// missing is a config whose source is not there, the fix that makes it
	// there (nil where none can), and what stops the fake; nil when the
	// plugin takes no source.
	missing func(dir string) (cfg map[string]string, fix func() error, stop func(), err error)
}

var subjects = []subject{
	{kind: "fs", fileTree: true, start: startFS, missing: missingFS},
	{kind: "proc", unixProc: true, start: startProc, missing: missingProc},
	{kind: "pages", start: func(string) (*source, error) { return &source{close: func() {}}, nil }},
	{kind: "gitlab", caches: true, start: startGitLab, missing: missingGitLab},
	{kind: "gmail", caches: true, start: startGmail, missing: missingGmail},
	{kind: "hey", caches: true, start: startHey, missing: missingHey},
}

func startFS(dir string) (*source, error) {
	root := filepath.Join(dir, "root")
	for path, body := range map[string]string{"a.md": "# Conformance\n", "sub/b.txt": "conformance\n"} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.Symlink(filepath.Join("sub", "b.txt"), filepath.Join(root, "link.txt")); err != nil {
		return nil, err
	}
	s := &source{
		cfg:     map[string]string{"root": root},
		meter:   &meter{read: inotifyWatches, level: true},
		breakIt: func() { _ = os.Chmod(root, 0) },
		close:   func() { _ = os.Chmod(root, 0o755) },
	}
	if os.Geteuid() != 0 { // root reads a directory whatever its mode
		s.outage = func(on bool) {
			mode := os.FileMode(0o755)
			if on {
				mode = 0
			}
			_ = os.Chmod(root, mode)
		}
	}
	return s, nil
}

func missingFS(dir string) (map[string]string, func() error, func(), error) {
	root := filepath.Join(dir, "absent")
	return map[string]string{"root": root}, func() error { return os.MkdirAll(root, 0o755) }, func() {}, nil
}

// startProc shows a shell the suite starts, holding one child, so the grid
// lists a process and its @info.
func startProc(string) (*source, error) {
	cmd := exec.Command("sh", "-c", "sleep 300 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so the stop reaches the child too
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }
	return &source{
		cfg: map[string]string{"pid": strconv.Itoa(cmd.Process.Pid)},
		// A poll of one shown process reads its stat and its children's,
		// hundreds of bytes a pass; an idle guest reads tens in the window.
		meter:   &meter{read: readBytes, slack: 1024},
		breakIt: stop,
		close:   stop,
	}, nil
}

func missingProc(string) (map[string]string, func() error, func(), error) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		return nil, nil, nil, err
	}
	return map[string]string{"pid": strconv.Itoa(cmd.ProcessState.Pid())}, nil, func() {}, nil
}

func tokenFile(dir, token string) (string, error) {
	p := filepath.Join(dir, "token")
	return p, os.WriteFile(p, []byte(token+"\n"), 0o600)
}

func startGitLab(dir string) (*source, error) {
	f := newGitLab()
	tok, err := tokenFile(dir, gitlabToken)
	if err != nil {
		f.close()
		return nil, err
	}
	return &source{
		// full_refresh this small walks on every read, so a read after an
		// outage starts asks GitLab again.
		cfg:     map[string]string{"url": f.srv.URL, "token_file": tok, "refresh": "100ms", "full_refresh": "1ms"},
		meter:   &meter{read: func(int) int64 { return f.count() }},
		outage:  func(on bool) { f.set(pick(on, down)) },
		revoke:  func(on bool) { f.set(pick(on, refusing)) },
		breakIt: func() { f.set(refusing) },
		close:   f.close,
	}, nil
}

func missingGitLab(dir string) (map[string]string, func() error, func(), error) {
	f := newGitLab()
	f.set(refusing)
	tok, err := tokenFile(dir, gitlabToken)
	return map[string]string{"url": f.srv.URL, "token_file": tok}, func() error { f.set(answering); return nil }, f.close, err
}

func startGmail(dir string) (*source, error) {
	f := newGmail()
	cfg, err := gmailConfig(dir, f)
	if err != nil {
		f.close()
		return nil, err
	}
	return &source{
		cfg:     cfg,
		meter:   &meter{read: func(int) int64 { return f.count() }},
		outage:  func(on bool) { f.set(pick(on, down)) },
		revoke:  func(on bool) { f.set(pick(on, refusing)) },
		breakIt: func() { f.set(refusing) },
		close:   f.close,
	}, nil
}

func missingGmail(dir string) (map[string]string, func() error, func(), error) {
	f := newGmail()
	f.set(refusing)
	cfg, err := gmailConfig(dir, f)
	return cfg, func() error { f.set(answering); return nil }, f.close, err
}

func startHey(dir string) (*source, error) {
	h, err := newHey(dir)
	if err != nil {
		return nil, err
	}
	return &source{
		cfg:     map[string]string{"binary": h.binary(), "refresh": "100ms"},
		meter:   &meter{read: func(int) int64 { return h.count() }},
		outage:  func(on bool) { h.set(pick(on, down)) },
		revoke:  func(on bool) { h.set(pick(on, refusing)) },
		breakIt: func() { _ = os.Remove(h.binary()) },
		close:   func() {},
	}, nil
}

func missingHey(dir string) (map[string]string, func() error, func(), error) {
	h := &heyFake{dir: dir}
	return map[string]string{"binary": h.binary()}, h.install, func() {}, nil
}

func pick(on bool, m sourceMode) sourceMode {
	if on {
		return m
	}
	return answering
}

// inotifyWatches counts the inotify instances pid holds: fs's OS change
// watch, which exists only while a stream's scope needs it.
func inotifyWatches(pid int) int64 {
	fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	var n int64
	for _, fd := range fds {
		if l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil && l == "anon_inode:inotify" {
			n++
		}
	}
	return n
}

// readBytes is the bytes pid has read through read(2) and its kin.
func readBytes(pid int) int64 {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, "rchar: "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// pluginRoot is the plugins checkout holding the built binaries.
func pluginRoot() (string, error) {
	if dir := os.Getenv("GRIDWELL_PLUGIN_DIR"); dir != "" {
		return dir, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.work above the test directory")
		}
		dir = parent
	}
}

// running is one spawned plugin.
type running struct {
	c    pluginv1.PluginClient
	pid  int
	kill func()
}

func spawn(bin string, cfg map[string]string, stateDir string) (*running, error) {
	full := map[string]string{"state_dir": stateDir}
	for k, v := range cfg {
		full[k] = v
	}
	p, err := compose.LoadPlugin(bin, full, io.Discard)
	if err != nil {
		return nil, err
	}
	pid, _ := strconv.Atoi(p.ID())
	return &running{c: pluginv1.NewPluginClient(p.Conn), pid: pid, kill: p.Kill}, nil
}

func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

func (r *running) info() (*pluginv1.InfoResponse, error) {
	ctx, cancel := call()
	defer cancel()
	return r.c.Info(ctx, &pluginv1.InfoRequest{})
}

func (r *running) list(grid string) (*pluginv1.ListResponse, error) {
	ctx, cancel := call()
	defer cancel()
	return r.c.List(ctx, &pluginv1.ListRequest{Grid: grid})
}

func gridKeys(info *pluginv1.InfoResponse) []string {
	var out []string
	for _, g := range info.Grids {
		out = append(out, g.Grid)
	}
	return out
}

// eventually polls f until it answers true or the bound passes. The harness
// times a plugin it cannot see into; the bound is what the plugin's own
// configured windows allow.
func eventually(bound time.Duration, f func() bool) bool {
	deadline := time.Now().Add(bound)
	for {
		if f() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// settle waits until the meter has not moved for half a second: the walks
// the suite's own calls started have landed.
func settle(m *meter, pid int) int64 {
	last := m.read(pid)
	stable := time.Now()
	for time.Since(stable) < 500*time.Millisecond {
		time.Sleep(100 * time.Millisecond)
		if now := m.read(pid); now != last {
			last, stable = now, time.Now()
		}
		if time.Since(stable) > callTimeout {
			break
		}
	}
	return last
}

// run is one subject's pass: the binary, a scratch directory, and what it
// found. A harness error is the suite failing to stage the check, never the
// plugin failing it.
type run struct {
	subject
	bin      string
	dir      string
	findings []string
	harness  []string
	staged   int
}

func (r *run) find(rule int, format string, args ...any) {
	r.findings = append(r.findings, fmt.Sprintf("rule %d: %s", rule, fmt.Sprintf(format, args...)))
}

func (r *run) fail(format string, args ...any) {
	r.harness = append(r.harness, fmt.Sprintf(format, args...))
}

// scratch is a directory no other staging of this run has used.
func (r *run) scratch(name string) string {
	r.staged++
	d := filepath.Join(r.dir, fmt.Sprintf("%s-%d", name, r.staged))
	if err := os.MkdirAll(d, 0o755); err != nil {
		r.fail("scratch %s: %v", name, err)
	}
	return d
}

// healthy spawns the plugin over a fresh source and passes Info.
func (r *run) healthy(name string) (*running, *source, *pluginv1.InfoResponse, func()) {
	dir := r.scratch(name)
	src, err := r.start(dir)
	if err != nil {
		r.fail("%s: start the source: %v", name, err)
		return nil, nil, nil, nil
	}
	p, err := spawn(r.bin, src.cfg, filepath.Join(dir, "state"))
	if err != nil {
		src.close()
		r.fail("%s: spawn: %v", name, err)
		return nil, nil, nil, nil
	}
	done := func() { p.kill(); src.close() }
	info, err := p.info()
	if err != nil {
		done()
		r.fail("%s: Info refused a healthy source: %v", name, err)
		return nil, nil, nil, nil
	}
	return p, src, info, done
}

func (r *run) checkInfoRefusesMissing() {
	if r.missing == nil {
		return
	}
	dir := r.scratch("missing")
	cfg, fix, stop, err := r.missing(dir)
	if stop != nil {
		defer stop()
	}
	if err != nil {
		r.fail("missing: stage: %v", err)
		return
	}
	p, err := spawn(r.bin, cfg, filepath.Join(dir, "state"))
	if err != nil {
		r.fail("missing: spawn: %v", err)
		return
	}
	defer p.kill()
	info, err := p.info()
	switch {
	case err == nil:
		r.find(2, "Info answered %d grids for a config whose source is not there", len(info.Grids))
		return
	case strings.TrimSpace(status.Convert(err).Message()) == "":
		r.find(2, "Info refused a missing source with no reason (%s)", status.Code(err))
	}
	if fix == nil {
		return
	}
	if err := fix(); err != nil {
		r.fail("missing: fix: %v", err)
		return
	}
	if _, err := p.info(); err != nil {
		r.find(2, "Info still refuses once the source is there, so the fix needs a restart: %v", err)
	}
}

func (r *run) checkLatch() {
	p, src, _, done := r.healthy("latch")
	if p == nil {
		return
	}
	defer done()
	if src.breakIt == nil {
		return
	}
	src.breakIt()
	if _, err := p.info(); err != nil {
		r.find(2, "Info passed, the source broke, and Info then refused: the verdict did not latch: %v", err)
	}
}

func (r *run) checkWatch() {
	p, src, info, done := r.healthy("watch")
	if p == nil {
		return
	}
	defer done()
	grids := gridKeys(info)
	for _, g := range grids {
		if _, err := p.list(g); err != nil {
			r.fail("watch: List %q: %v", g, err)
		}
	}
	var before int64
	if src.meter != nil {
		before = settle(src.meter, p.pid)
		time.Sleep(idleWindow)
		if after := src.meter.read(p.pid); !src.meter.quiet(before, after) {
			r.find(8, "with no Watch held the plugin worked its source over %s: %d to %d", idleWindow, before, after)
		}
		before = src.meter.read(p.pid)
	}

	if !info.Watch {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := p.c.Watch(ctx, &pluginv1.WatchRequest{Grids: grids})
		if err == nil {
			_, err = s.Recv()
		}
		if status.Code(err) != codes.Unimplemented {
			r.find(1, "Info declares no Watch, but Watch answered %v rather than Unimplemented", status.Code(err))
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := p.c.Watch(ctx, &pluginv1.WatchRequest{Grids: grids})
	if err != nil {
		r.fail("watch: open: %v", err)
		return
	}
	headed := make(chan error, 1)
	go func() { _, err := stream.Header(); headed <- err }()
	select {
	case err := <-headed:
		if err != nil {
			r.find(9, "a declared Watch sent no header: %v", err)
			return
		}
	case <-time.After(5 * time.Second):
		r.find(9, "a declared Watch sent no header within 5s of accepting the stream")
		return
	}
	removed := make(chan string, 1)
	go func() {
		for {
			ch, err := stream.Recv()
			if err != nil {
				return
			}
			if rm := ch.GetEntryRemoved(); rm != nil {
				select {
				case removed <- rm.Grid + "/" + rm.Key:
				default:
				}
			}
		}
	}()
	if r.kind == "fs" {
		_ = os.WriteFile(filepath.Join(src.cfg["root"], "new.txt"), []byte("x"), 0o644)
		_ = os.Remove(filepath.Join(src.cfg["root"], "a.md"))
	}
	if src.meter != nil && !eventually(6*time.Second, func() bool { return src.meter.busy(before, src.meter.read(p.pid)) }) {
		r.fail("watch: the plugin did no source work under a Watch either, so the idle check proves nothing")
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case at := <-removed:
		r.find(18, "a Watch stream sent the retired EntryRemoved (%s)", at)
	default:
	}
}

// checkUnreachable stages each failure the source can have after Info passed
// and reads every declared grid cold.
func (r *run) checkUnreachable() {
	for _, c := range []struct {
		what  string
		stage func(*source) func(bool)
	}{
		{"an outage", func(s *source) func(bool) { return s.outage }},
		{"a refused credential", func(s *source) func(bool) { return s.revoke }},
	} {
		p, src, info, done := r.healthy("unreachable")
		if p == nil {
			return
		}
		set := c.stage(src)
		if set == nil {
			done()
			continue
		}
		set(true)
		for _, g := range gridKeys(info) {
			var verdict error
			said := eventually(callTimeout, func() bool {
				resp, err := p.list(g)
				switch {
				case status.Code(err) == codes.Unavailable:
					return true
				case err != nil:
					verdict = err
					return true
				case resp.Unreachable != "":
					return true
				case resp.Authoritative:
					verdict = fmt.Errorf("an authoritative listing of %d entries", len(resp.Entries))
					return true
				}
				return false
			})
			switch {
			case verdict != nil:
				r.find(3, "after Info passed, %s read grid %q cold as %v, not Unavailable", c.what, g, verdict)
			case !said:
				r.find(3, "after Info passed, %s read grid %q as an empty listing that said nothing", c.what, g)
			default:
				continue
			}
			break
		}
		done()
	}
}

// checkCacheAnswer warms a grid, stages an outage, and reads it again.
func (r *run) checkCacheAnswer() {
	if !r.caches {
		return
	}
	p, src, info, done := r.healthy("cache")
	if p == nil {
		return
	}
	defer done()
	if src.outage == nil {
		return
	}
	grid := ""
	for _, g := range gridKeys(info) {
		if eventually(callTimeout, func() bool { resp, err := p.list(g); return err == nil && len(resp.Entries) > 0 }) {
			grid = g
			break
		}
	}
	if grid == "" {
		r.fail("cache: no declared grid ever listed an entry to warm")
		return
	}
	warm, _ := p.list(grid)
	src.outage(true)
	var got *pluginv1.ListResponse
	var failed error
	eventually(15*time.Second, func() bool {
		resp, err := p.list(grid)
		if err != nil {
			failed = err
			return true
		}
		got = resp
		return resp.Unreachable != ""
	})
	switch {
	case failed != nil:
		r.find(7, "a warm read of %q during an outage failed (%v) where the cache could answer", grid, failed)
	case got.Unreachable == "":
		r.find(7, "a warm read of %q during an outage never said so in unreachable", grid)
	case len(got.Entries) < len(warm.Entries):
		r.find(7, "a warm read of %q during an outage answered %d entries of the %d cached", grid, len(got.Entries), len(warm.Entries))
	case got.Authoritative:
		r.find(7, "a cache answer of %q claimed authoritative", grid)
	}
}

// listed is every entry of the declared grids and of the grids they open,
// one level down, by grid.
func listed(p *running, info *pluginv1.InfoResponse) map[string][]*pluginv1.Entry {
	out := map[string][]*pluginv1.Entry{}
	var walk func(g string, depth int)
	walk = func(g string, depth int) {
		if _, seen := out[g]; seen {
			return
		}
		resp, err := p.list(g)
		if err != nil {
			out[g] = nil
			return
		}
		out[g] = resp.Entries
		if depth == 0 {
			return
		}
		for _, e := range resp.Entries {
			if e.ChildGrid != "" && e.LinkTarget == nil {
				walk(e.ChildGrid, depth-1)
			}
		}
	}
	for _, g := range gridKeys(info) {
		walk(g, 1)
	}
	return out
}

// checkEntries reads every listed entry for rules 5, 11 and 12.
func (r *run) checkEntries() {
	p, _, info, done := r.healthy("entries")
	if p == nil {
		return
	}
	defer done()
	// A cold read answers what the cache holds by its first-answer bound;
	// the second read is the walk landed.
	listed(p, info)
	grids := listed(p, info)
	names := make([]string, 0, len(grids))
	for g := range grids {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		for _, e := range grids[g] {
			if e.Kind == rpc.KindText && e.LinkTarget == nil &&
				e.TextPresentation != rpc.TextPresentationPlain && e.TextPresentation != rpc.TextPresentationBoth {
				r.find(12, "text entry %q in grid %q declares text_presentation %q", e.Key, g, e.TextPresentation)
			}
			if e.StatusDetail != "" && !oneSymbol(e.StatusDetail) {
				r.find(11, "entry %q in grid %q carries status_detail %q, not one symbol", e.Key, g, e.StatusDetail)
			}
			if t := e.LinkTarget; t != nil {
				r.checkLink(p, grids, g, e, t)
			}
		}
	}
}

func (r *run) checkLink(p *running, grids map[string][]*pluginv1.Entry, g string, e *pluginv1.Entry, t *pluginv1.EntryRef) {
	if t.Grid == "" || t.Key == "" {
		r.find(5, "entry %q in grid %q links to grid %q key %q: a link_target names both", e.Key, g, t.Grid, t.Key)
		return
	}
	entries, ok := grids[t.Grid]
	if !ok {
		resp, err := p.list(t.Grid)
		if err != nil {
			r.find(5, "entry %q in grid %q links into grid %q, which does not list: %v", e.Key, g, t.Grid, err)
			return
		}
		entries = resp.Entries
		grids[t.Grid] = entries
	}
	for _, home := range entries {
		if home.Key == t.Key {
			if home.LinkTarget != nil {
				r.find(5, "entry %q in grid %q links to %q in %q, which is itself a link", e.Key, g, t.Key, t.Grid)
			}
			return
		}
	}
	r.find(5, "entry %q in grid %q links to key %q, which grid %q does not list", e.Key, g, t.Key, t.Grid)
}

// oneSymbol is one emoji or symbol, with its variation selector: never a
// letter, a digit or a word.
func oneSymbol(s string) bool {
	n := 0
	for _, c := range s {
		switch {
		case c == '︎' || c == '️':
		case unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.IsSpace(c):
			return false
		default:
			n++
		}
	}
	return n == 1
}

// checkFind asks Find after a warm read and counts what it asks the source.
func (r *run) checkFind() {
	p, src, info, done := r.healthy("find")
	if p == nil {
		return
	}
	defer done()
	listed(p, info)
	var before int64
	if src.meter != nil {
		before = settle(src.meter, p.pid)
	}
	ctx, cancel := call()
	defer cancel()
	_, err := p.c.Find(ctx, &pluginv1.FindRequest{Query: "Conformance", Limit: 10})
	if r.fileTree {
		if status.Code(err) != codes.Unimplemented {
			r.find(20, "a file tree answered Find with %v, not Unimplemented", status.Code(err))
		} else if msg := status.Convert(err).Message(); msg == "" || strings.HasPrefix(msg, "method Find not implemented") {
			r.find(20, "a file tree's Find refusal gives no reason of its own (%q)", msg)
		}
		return
	}
	if status.Code(err) == codes.Unimplemented || src.meter == nil || src.meter.level {
		return
	}
	time.Sleep(500 * time.Millisecond)
	if after := src.meter.read(p.pid); after != before {
		r.find(20, "Find asked the source %d times; it answers from the cache only", after-before)
	}
}

// checkStaleWrite writes a text entry claiming a stamp it never had.
func (r *run) checkStaleWrite() {
	p, _, info, done := r.healthy("write")
	if p == nil {
		return
	}
	defer done()
	if !info.Writable {
		return
	}
	var target *pluginv1.Entry
	for _, entries := range listed(p, info) {
		for _, e := range entries {
			if e.Kind == rpc.KindText && e.LinkTarget == nil && e.ContentStamp != "" && e.ReadOnly == "" && target == nil {
				target = e
			}
		}
	}
	if target == nil {
		r.fail("write: a writable plugin listed no text entry with a stamp to write")
		return
	}
	before, err := r.read(p, target.Key)
	if err != nil {
		r.fail("write: read %q: %v", target.Key, err)
		return
	}
	ctx, cancel := call()
	defer cancel()
	ws, err := p.c.WriteContent(ctx)
	if err == nil {
		err = ws.Send(&pluginv1.WriteContentRequest{Key: target.Key, Data: []byte("conformance overwrite\n"), ContentStamp: "conformance-stale"})
	}
	if err == nil {
		_, err = ws.CloseAndRecv()
	}
	if status.Code(err) != codes.FailedPrecondition {
		r.find(19, "a write of %q claiming a stale stamp answered %v, not FailedPrecondition", target.Key, status.Code(err))
	}
	if after, err := r.read(p, target.Key); err != nil || !bytes.Equal(before, after) {
		r.find(19, "a write of %q claiming a stale stamp changed its bytes", target.Key)
	}
}

func (r *run) read(p *running, key string) ([]byte, error) {
	ctx, cancel := call()
	defer cancel()
	s, err := p.c.ReadContent(ctx, &pluginv1.ReadContentRequest{Key: key})
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		chunk, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, chunk.Data...)
	}
}

// verdict is the suite's answer for one plugin.
type verdict struct {
	kind              string
	findings, harness []string
	skipped           string
}

var (
	suiteOnce sync.Once
	suite     []verdict
)

// conformance runs every check against every shipped binary once per test
// binary, the subjects in parallel. The fs plugin trashes into the home, so
// the home is a scratch directory for the run.
func conformance() []verdict {
	suiteOnce.Do(func() {
		root, err := pluginRoot()
		scratch, terr := os.MkdirTemp("", "conformance-")
		if err == nil {
			err = terr
		}
		if err != nil {
			suite = []verdict{{kind: "harness", harness: []string{err.Error()}}}
			return
		}
		defer os.RemoveAll(scratch)
		for _, k := range []string{"HOME", "XDG_DATA_HOME"} {
			old, had := os.LookupEnv(k)
			_ = os.Setenv(k, filepath.Join(scratch, "home"))
			if had {
				defer os.Setenv(k, old)
			} else {
				defer os.Unsetenv(k)
			}
		}
		suite = make([]verdict, len(subjects))
		var wg sync.WaitGroup
		for i, s := range subjects {
			v := &suite[i]
			v.kind = s.kind
			if s.unixProc && runtime.GOOS != "linux" {
				v.skipped = "reads /proc"
				continue
			}
			if runtime.GOOS != "linux" {
				// The meters read /proc; elsewhere the suite has no eyes.
				v.skipped = "the meters read /proc"
				continue
			}
			bin := filepath.Join(root, "gridwell-plugin-"+s.kind)
			if _, err := os.Stat(bin); err != nil {
				v.harness = []string{fmt.Sprintf("%s not found: run `make build` first, or set GRIDWELL_PLUGIN_DIR", bin)}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := &run{subject: s, bin: bin, dir: filepath.Join(scratch, s.kind)}
				r.checkInfoRefusesMissing()
				r.checkLatch()
				r.checkWatch()
				r.checkUnreachable()
				r.checkCacheAnswer()
				r.checkEntries()
				r.checkFind()
				r.checkStaleWrite()
				v.findings, v.harness = r.findings, r.harness
			}()
		}
		wg.Wait()
	})
	return suite
}

// TestConformance fails on every finding, under its plugin, and wherever the
// suite cannot stage a check.
func TestConformance(t *testing.T) {
	for _, v := range conformance() {
		t.Run(v.kind, func(t *testing.T) {
			if v.skipped != "" {
				t.Skipf("not checked on %s: %s", runtime.GOOS, v.skipped)
			}
			for _, h := range v.harness {
				t.Error(h)
			}
			for _, f := range v.findings {
				t.Error(f)
			}
		})
	}
}

// The status-detail rule's reading of "one emoji".
func TestOneSymbol(t *testing.T) {
	for s, want := range map[string]bool{"✅": true, "⏸️": true, "●": true, "💀": true, "done": false, "● 2": false, "": false, "✅✅": false} {
		if got := oneSymbol(s); got != want {
			t.Errorf("oneSymbol(%q) = %v, want %v", s, got, want)
		}
	}
}
