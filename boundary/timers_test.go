package boundary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The timer list is permanent: every clock the production tree starts is
// named in allowed-timers.txt with its class and reason, as gridwell's
// scripts/allowed-timers.txt names its own, so the count of unlisted timers
// is held at zero by a test rather than by a ledger row.

// clockCalls are the waits on a clock: the time package's, and the same
// methods called on a memo.Clock (an identifier ending in clock or Clock).
var clockCalls = map[string]bool{"NewTicker": true, "Tick": true, "After": true, "NewTimer": true, "AfterFunc": true, "Sleep": true}

// timerClasses are why a clock may run: a debounce coalesces bursts, a
// backoff paces a retry, a bound caps a wait that something else ends, and a
// poll asks a source that cannot tell.
var timerClasses = map[string]bool{"debounce": true, "backoff": true, "bound": true, "poll": true}

var identifier = regexp.MustCompile(`[A-Za-z_$][\w$]*`)

type timerSite struct {
	rel  string
	line int
	fn   string // the enclosing function
	near string // the site's line and the one above it
}

// anchoredBy reports whether an allowed-timers anchor names this site: its
// line number, its enclosing function, or a word on its line or the line
// above, so an entry survives edits elsewhere in the file.
func (s timerSite) anchoredBy(anchor string) bool {
	if n, err := strconv.Atoi(anchor); err == nil {
		return n == s.line
	}
	if anchor == s.fn {
		return true
	}
	for _, w := range identifier.FindAllString(s.near, -1) {
		if w == anchor {
			return true
		}
	}
	return false
}

func timerSites(t *testing.T, root string) []timerSite {
	var out []timerSite
	walkRepo(t, root, func(rel, path string, data []byte) {
		if !strings.HasSuffix(rel, ".go") || isTestFile(rel) || !isProduction(rel) {
			return
		}
		lines := strings.Split(string(data), "\n")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isClockAdapter(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if ok && isTimerCall(call) {
					line := fset.Position(call.Pos()).Line
					near := lines[line-1]
					if line > 1 {
						near = lines[line-2] + "\n" + near
					}
					out = append(out, timerSite{rel, line, fn.Name.Name, near})
				}
				return true
			})
		}
	})
	return out
}

// isTimerCall is a time package wait, the same wait through a memo.Clock, or
// memo.Poll, which starts a clock of its own for each caller.
func isTimerCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	var recv string
	switch x := sel.X.(type) {
	case *ast.Ident:
		recv = x.Name
	case *ast.SelectorExpr:
		recv = x.Sel.Name
	}
	switch {
	case recv == "time":
		return clockCalls[sel.Sel.Name]
	case recv == "memo":
		return sel.Sel.Name == "Poll"
	}
	return clockCalls[sel.Sel.Name] && strings.HasSuffix(strings.ToLower(recv), "clock")
}

// isClockAdapter is a method that only hands the time package's wait of its
// own name back, a memo.Clock over the wall clock: the timer starts where the
// adapter is called, which is the site listed.
func isClockAdapter(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "time" && sel.Sel.Name == fn.Name.Name
}

type allowedTimer struct {
	at, rel, anchor, class, reason string
}

func allowedTimers(t *testing.T, root string) []allowedTimer {
	t.Helper()
	var out []allowedTimer
	for _, l := range allowLines(t, filepath.Join(root, "boundary", "allowed-timers.txt")) {
		fields := strings.Split(l, "|")
		if len(fields) != 3 {
			t.Errorf("allowed-timers.txt: %q: want path:anchor | class | reason", l)
			continue
		}
		where := strings.TrimSpace(fields[0])
		i := strings.LastIndex(where, ":")
		class, reason := strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
		switch {
		case i < 0:
			t.Errorf("allowed-timers.txt: %q names no anchor", where)
			continue
		case !timerClasses[class]:
			t.Errorf("allowed-timers.txt: %s: class %q is not debounce, backoff, bound or poll", where, class)
		case reason == "":
			t.Errorf("allowed-timers.txt: %s gives no reason", where)
		case class == "poll" && !strings.Contains(reason, "cannot tell"):
			t.Errorf("allowed-timers.txt: %s is a poll whose reason does not say which source cannot tell", where)
		}
		out = append(out, allowedTimer{where, filepath.FromSlash(where[:i]), where[i+1:], class, reason})
	}
	return out
}

// unlistedTimers are the timer sites allowed-timers.txt does not name.
func unlistedTimers(t *testing.T, root string) []string {
	allowed := allowedTimers(t, root)
	var out []string
	for _, s := range timerSites(t, root) {
		listed := false
		for _, a := range allowed {
			if a.rel == s.rel && s.anchoredBy(a.anchor) {
				listed = true
				break
			}
		}
		if !listed {
			out = append(out, fmt.Sprintf("%s:%d", filepath.ToSlash(s.rel), s.line))
		}
	}
	return out
}

// A clock runs only for a reason someone wrote down: the node learns of a
// change by being told, so a timer that is none of debounce, backoff, bound
// or poll-of-a-source-that-cannot-tell is the clock the promise forbids.
func TestEveryTimerIsListed(t *testing.T) {
	for _, at := range unlistedTimers(t, repoRoot(t)) {
		t.Errorf("%s starts a timer boundary/allowed-timers.txt does not list: list it as path:anchor | class | reason, or remove it", at)
	}
}

// An allowed timer names a site that still exists, so a deleted timer's entry
// cannot vouch for the next one written beside it.
func TestAllowedTimersAreLive(t *testing.T) {
	root := repoRoot(t)
	sites := timerSites(t, root)
	for _, a := range allowedTimers(t, root) {
		live := false
		for _, s := range sites {
			if a.rel == s.rel && s.anchoredBy(a.anchor) {
				live = true
				break
			}
		}
		if !live {
			t.Errorf("allowed-timers.txt: %s anchors no timer; delete the entry", a.at)
		}
	}
}

func TestIsTimerCall(t *testing.T) {
	src := `package p
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func f() {
	time.Sleep(1)
	c.o.Clock.After(1)
	clock.After(1)
	memo.Poll(ctx, clock, 1, nil)
	t.CreatedAt.After(u)
	weeks[i].After(weeks[j])
}`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !isClockAdapter(file.Decls[0].(*ast.FuncDecl)) {
		t.Error("a method handing back time.After is a clock adapter, not a site")
	}
	var got []bool
	for _, st := range file.Decls[1].(*ast.FuncDecl).Body.List {
		got = append(got, isTimerCall(st.(*ast.ExprStmt).X.(*ast.CallExpr)))
	}
	want := []bool{true, true, true, true, false, false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("isTimerCall = %v, want %v", got, want)
	}
}
