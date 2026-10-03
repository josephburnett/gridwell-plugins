package procsource

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeRawStatus drops a status file with arbitrary content for one pid.
func writeRawStatus(t *testing.T, root string, pid int64, status string) {
	t.Helper()
	dir := filepath.Join(root, strconv.FormatInt(pid, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGetToleratesMalformedStatus: /proc/<pid>/status is kernel-formatted but
// can race or vary; the parser must not error on lines without a colon, blank
// lines, or non-numeric numeric fields. It parses what it can and zero-fills
// the rest — a process is never dropped from the grid just because one field
// was unparseable.
func TestGetToleratesMalformedStatus(t *testing.T) {
	root := t.TempDir()
	const pid = 42
	writeRawStatus(t, root, pid, ""+
		"Name:\tweird proc\n"+
		"this line has no colon and must be ignored\n"+
		"\n"+ // blank line
		"PPid:\tnot-a-number\n"+ // non-numeric → field stays 0
		"State:\tD (disk sleep)\n"+
		"VmRSS:\t  2048 kB\n"+ // leading spaces + unit
		"Uid:\t1000\t1000\t1000\t1000\n")

	info, err := Get(root, pid)
	if err != nil {
		t.Fatalf("Get on malformed-but-present status should not error: %v", err)
	}
	if info.Name != "weird proc" {
		t.Errorf("Name = %q, want %q", info.Name, "weird proc")
	}
	if info.PPID != 0 {
		t.Errorf("PPID from non-numeric value = %d, want 0", info.PPID)
	}
	if info.State != "D (disk sleep)" {
		t.Errorf("State = %q", info.State)
	}
	if info.VmRSSKB != 2048 {
		t.Errorf("VmRSS = %d, want 2048", info.VmRSSKB)
	}
	if info.UID != 1000 {
		t.Errorf("UID = %d, want 1000 (first field)", info.UID)
	}
}

// TestExistsDistinguishesGoneFromUncertain: the proc plugin's non-authoritative
// sweep deletes a tile only on a DEFINITIVE (false, nil) — "the pid is gone".
// An I/O error must surface as (false, non-nil) "uncertain" so the sweep keeps
// the tile instead of dropping a process that may still be alive. A procRoot
// that is a regular file makes Stat(procRoot/<pid>) fail with ENOTDIR (not
// NotExist), exercising exactly that uncertain branch.
func TestExistsDistinguishesGoneFromUncertain(t *testing.T) {
	root := t.TempDir()
	// Definitively gone: pid dir simply absent.
	if ok, err := Exists(root, 99999); ok || err != nil {
		t.Errorf("absent pid = (%v,%v), want (false,nil) definitively gone", ok, err)
	}
	// Uncertain: procRoot is a file, so the stat errors with something other
	// than NotExist.
	fileRoot := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(fileRoot, 1); ok || err == nil {
		t.Errorf("unreadable root = (%v,%v), want (false, non-nil) uncertain", ok, err)
	}
}

// TestSplitKVAndFirstField pin the two pure string helpers directly.
func TestSplitKVAndFirstField(t *testing.T) {
	if k, v, ok := splitKV("PPid:\t100"); !ok || k != "PPid" || v != "100" {
		t.Errorf("splitKV = %q,%q,%v", k, v, ok)
	}
	if _, _, ok := splitKV("no colon here"); ok {
		t.Error("a line with no colon should report ok=false")
	}
	if got := firstField("1000\t1000\t1000"); got != "1000" {
		t.Errorf("firstField = %q, want 1000", got)
	}
	if got := firstField("   "); got != "" {
		t.Errorf("firstField(blank) = %q, want empty", got)
	}
}

// A command name is the process's to choose: parseStat takes the fields after
// the last ')', so spaces and parentheses in the name do not shift them.
func TestParseStatSplitsAfterTheLastParen(t *testing.T) {
	for line, want := range map[string]Stat{
		statLine(7, "sh", 3, 55):            {PID: 7, PPID: 3, StartTime: 55},
		statLine(9, "a) b) (c", 4, 66):      {PID: 9, PPID: 4, StartTime: 66},
		statLine(12, "tmux: server", 1, 77): {PID: 12, PPID: 1, StartTime: 77},
	} {
		got, err := parseStat([]byte(line))
		if err != nil || got != want {
			t.Errorf("parseStat(%q) = %+v, %v; want %+v", line, got, err, want)
		}
	}
	for _, bad := range []string{"", "7 sh S 3", "7 (sh) S", "x (sh) S 3"} {
		if got, err := parseStat([]byte(bad)); err == nil {
			t.Errorf("parseStat(%q) = %+v; want an error", bad, got)
		}
	}
}
