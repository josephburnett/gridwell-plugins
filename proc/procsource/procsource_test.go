package procsource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildrenAndGet(t *testing.T) {
	root := stubProc(t, []stubProcess{
		{pid: 1, ppid: 0, name: "init", cmdline: "/sbin/init"},
		{pid: 2, ppid: 1, name: "kthreadd", cmdline: ""},
		{pid: 100, ppid: 1, name: "bash", cmdline: "/bin/bash -i"},
		{pid: 200, ppid: 100, name: "vim", cmdline: "vim /tmp/file"},
		{pid: 201, ppid: 100, name: "grep", cmdline: "grep -r needle"},
	})

	ctx := context.Background()
	kids, err := Children(ctx, root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := pidsOf(kids); !equalInt64(got, []int64{2, 100}) {
		t.Errorf("Children(1) pids = %v, want [2 100]", got)
	}

	kids100, err := Children(ctx, root, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := pidsOf(kids100); !equalInt64(got, []int64{200, 201}) {
		t.Errorf("Children(100) pids = %v, want [200 201]", got)
	}

	info, err := Get(root, 200)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "vim" || info.PPID != 100 || info.CmdLine != "vim /tmp/file" {
		t.Errorf("Get(200) = %+v", info)
	}
}

func TestChildrenSkipsUnreadablePIDs(t *testing.T) {
	root := stubProc(t, []stubProcess{
		{pid: 1, ppid: 0, name: "init", cmdline: "/sbin/init"},
	})
	// A pid dir with no stat file is a process that exited between the
	// listing and the read.
	if err := os.Mkdir(filepath.Join(root, "999"), 0o755); err != nil {
		t.Fatal(err)
	}
	kids, err := Children(context.Background(), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].PID != 1 {
		t.Errorf("expected only pid 1, got %+v", kids)
	}
}

// Children reads every process on the host to find a few, so it reads only
// stat: a process whose status, cmdline and cwd are absent is still listed,
// with its start time.
func TestChildrenReadsOnlyStat(t *testing.T) {
	root := stubProc(t, []stubProcess{{pid: 1, ppid: 0, name: "init"}, {pid: 7, ppid: 1, name: "sh"}})
	for _, f := range []string{"status", "cmdline"} {
		if err := os.Remove(filepath.Join(root, "7", f)); err != nil {
			t.Fatal(err)
		}
	}
	kids, err := Children(context.Background(), root, 1)
	if err != nil || len(kids) != 1 || kids[0] != (Stat{PID: 7, PPID: 1, StartTime: 1007}) {
		t.Errorf("Children(1) = %+v, %v; want pid 7 from its stat alone", kids, err)
	}
}

// The scan's own failures are Children's error, never a short listing: a
// table it cannot list, and a request abandoned mid-scan.
func TestChildrenReturnsScanErrors(t *testing.T) {
	root := stubProc(t, []stubProcess{{pid: 1, ppid: 0, name: "init"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if kids, err := Children(ctx, root, 0); err != context.Canceled {
		t.Errorf("abandoned scan = %+v, %v; want context.Canceled", kids, err)
	}
	if kids, err := Children(context.Background(), filepath.Join(root, "absent"), 0); err == nil {
		t.Errorf("unlistable table = %+v; want an error", kids)
	}
}

func TestGetMissingPID(t *testing.T) {
	root := stubProc(t, nil)
	if _, err := Get(root, 42); err == nil {
		t.Error("expected error for missing pid")
	}
}

// The stub's stat lines are this package's guess at the kernel's; the real
// table checks the guess: this test process is a child of its parent.
func TestChildrenOnTheRealTable(t *testing.T) {
	kids, err := Children(context.Background(), DefaultRoot, int64(os.Getppid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kids {
		if k.PID == int64(os.Getpid()) {
			if k.StartTime == 0 {
				t.Errorf("pid %d has no start time: %+v", k.PID, k)
			}
			return
		}
	}
	t.Errorf("Children(%d) = %+v; want this process, %d", os.Getppid(), kids, os.Getpid())
}

func TestExists(t *testing.T) {
	root := stubProc(t, []stubProcess{{pid: 1, ppid: 0, name: "init"}})

	// Present process → (true, nil).
	if ok, err := Exists(root, 1); err != nil || !ok {
		t.Errorf("Exists(1) = (%v, %v), want (true, nil)", ok, err)
	}
	// Definitively absent → (false, nil) — this is the only "gone" answer,
	// the one the reconciler treats as grounds to sweep a tile.
	if ok, err := Exists(root, 42); err != nil || ok {
		t.Errorf("Exists(42) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestMetadataMarkdownDeterministic(t *testing.T) {
	info := Info{PID: 100, PPID: 1, Name: "bash", CmdLine: "/bin/bash", UID: 1000}
	a := MetadataMarkdown(info)
	b := MetadataMarkdown(info)
	if a != b {
		t.Error("non-deterministic")
	}
	if !strings.Contains(a, "bash") || !strings.Contains(a, "pid: 100") {
		t.Errorf("missing fields:\n%s", a)
	}
}

func TestMetadataMarkdownKernelThread(t *testing.T) {
	// Kernel threads have empty cmdline — the "- cmd:" line should not
	// appear at all rather than rendering an empty backtick block.
	info := Info{PID: 2, PPID: 0, Name: "kthreadd", CmdLine: "", UID: 0}
	md := MetadataMarkdown(info)
	if strings.Contains(md, "cmd: ``") {
		t.Errorf("empty cmdline should be omitted:\n%s", md)
	}
}

type stubProcess struct {
	pid, ppid int64
	name      string
	cmdline   string
}

// stubProc writes a fake /proc directory under t.TempDir() and returns
// its path. Each pid gets a stat file, a status file (Name/PPid/Uid) and a
// cmdline file (NUL-separated like the real /proc).
func stubProc(t *testing.T, procs []stubProcess) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprintf("%d", p.pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("Name:\t%s\nPPid:\t%d\nUid:\t1000\t1000\t1000\t1000\n", p.name, p.ppid)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(p.pid, p.name, p.ppid, 1000+uint64(p.pid))), 0o644); err != nil {
			t.Fatal(err)
		}
		// cmdline: replace spaces with NULs and add a trailing NUL like
		// the kernel does.
		var cl []byte
		for _, ch := range []byte(p.cmdline) {
			if ch == ' ' {
				cl = append(cl, 0)
			} else {
				cl = append(cl, ch)
			}
		}
		if len(cl) > 0 {
			cl = append(cl, 0)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), cl, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Add a non-numeric subdir to make sure listPIDs filters it.
	if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// statLine is /proc/<pid>/stat as the kernel writes it, to starttime.
func statLine(pid int64, name string, ppid int64, start uint64) string {
	return fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, name, ppid, pid, pid, start)
}

func pidsOf(infos []Stat) []int64 {
	out := make([]int64, len(infos))
	for i, info := range infos {
		out[i] = info.PID
	}
	return out
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
