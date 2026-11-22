package main

import (
	"errors"
	"flag"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// setVar replaces one of the seams in main.go for the length of a test.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// fakeHome makes home() answer dir whatever the machine's passwd says.
func fakeHome(t *testing.T, dir string) {
	t.Helper()
	setVar(t, &currentUser, func() (*user.User, error) { return &user.User{HomeDir: dir}, nil })
}

// quiet runs fn with stderr captured, and returns what it said there.
func quiet(t *testing.T, fn func()) string { return cov43CaptureStderr(t, fn) }

func TestHomePrefersThePasswdEntryAndFallsBackToHOME(t *testing.T) {
	t.Setenv("HOME", "/from-env")
	cases := []struct {
		name string
		u    *user.User
		err  error
		want string
	}{
		{"entry", &user.User{HomeDir: "/from-passwd"}, nil, "/from-passwd"},
		{"lookup fails", &user.User{HomeDir: "/ignored"}, errors.New("no passwd entry"), "/from-env"},
		{"entry without a home", &user.User{}, nil, "/from-env"},
	}
	for _, c := range cases {
		setVar(t, &currentUser, func() (*user.User, error) { return c.u, c.err })
		if got := home(); got != c.want {
			t.Errorf("%s: home() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRootDefaultsUnderHomeOutsideATestBinary(t *testing.T) {
	t.Setenv("AGENTSYNC_DIR", "")
	h := t.TempDir()
	fakeHome(t, h)

	setVar(t, &underTest, func() bool { return false })
	if got, want := root(), filepath.Join(h, ".claude", "agentsync"); got != want {
		t.Errorf("root() = %q, want %q", got, want)
	}

	// Under a test binary the same environment is refused, loudly.
	underTest = func() bool { return true }
	defer func() {
		if r := recover(); r == nil {
			t.Error("the default path was reachable under a test binary")
		}
	}()
	root()
}

func TestOwnerOrderAndHostnameFallback(t *testing.T) {
	t.Setenv("AGENTSYNC_OWNER", "explicit")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session")
	if got := owner(); got != "explicit" {
		t.Errorf("owner() = %q, want the explicit one", got)
	}
	t.Setenv("AGENTSYNC_OWNER", "")
	if got := owner(); got != "session" {
		t.Errorf("owner() = %q, want the session id", got)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	h, _ := os.Hostname()
	if got := owner(); got != "unidentified-"+h {
		t.Errorf("owner() = %q, want unidentified-%s", got, h)
	}
}

func TestMemoryIndexWithoutATranscriptFollowsTheWorkingDirectory(t *testing.T) {
	t.Setenv("AGENTSYNC_MEMORY", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	h := t.TempDir()
	fakeHome(t, h)

	setVar(t, &getwd, func() (string, error) { return "/Users/some_one/work", nil })
	want := filepath.Join(h, ".claude", "projects", "-Users-some-one-work", "memory", "MEMORY.md")
	if got := memoryIndex(); got != want {
		t.Errorf("memoryIndex() = %q, want %q", got, want)
	}

	getwd = func() (string, error) { return "", errors.New("the directory is gone") }
	if got := memoryIndex(); got != "" {
		t.Errorf("memoryIndex() = %q with no working directory, want empty", got)
	}
}

func TestSessionProjectDirRefusesAPatternThatCannotBeGlobbed(t *testing.T) {
	if _, err := sessionProjectDir("[", "abc"); err == nil {
		t.Error("a malformed projects directory was accepted")
	}
}

func TestRecordsFailOutLoud(t *testing.T) {
	t.Run("a record file that cannot be read", func(t *testing.T) {
		sandbox(t, "me-1")
		if err := os.MkdirAll(recordPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := loadRecords(); err == nil {
			t.Error("a directory where the records belong was read as empty")
		}
	})
	t.Run("a directory that cannot be made", func(t *testing.T) {
		cov43FileForStore(t, "me-1")
		if err := saveRecords([]record{{Name: "x"}}); err == nil {
			t.Error("saved under a file")
		}
	})
	t.Run("a record that cannot be encoded", func(t *testing.T) {
		sandbox(t, "me-1")
		bad := record{Name: "x", Added: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
		if err := saveRecords([]record{bad}); err == nil {
			t.Error("a year json cannot hold was saved")
		}
	})
	t.Run("a temporary file that cannot be written", func(t *testing.T) {
		sandbox(t, "me-1")
		if err := os.MkdirAll(recordPath()+".tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := saveRecords([]record{{Name: "x"}}); err == nil {
			t.Error("wrote through a directory")
		}
	})
}

func TestWithIndexLeaseRefusesWhileAnotherSessionEdits(t *testing.T) {
	sandbox(t, "me-1")
	if _, err := store().Acquire("MEMORY.md", "other-2", "index edit", time.Hour); err != nil {
		t.Fatal(err)
	}
	ran := false
	err := withIndexLease(func() error { ran = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "MEMORY.md is being edited") {
		t.Fatalf("got %v, want the being-edited refusal", err)
	}
	if ran {
		t.Error("the edit ran without the lease")
	}
	var held *lease.Held
	if !errors.As(err, &held) {
		t.Error("the refusal does not carry who holds it")
	}
}

func TestUsageErrorUnwrapsToItsCause(t *testing.T) {
	cause := errors.New("the cause")
	if got := errors.Unwrap(usageError{cause}); got != cause {
		t.Errorf("Unwrap = %v, want the cause", got)
	}
}

func TestLineFromFrontmatterFallsBackToTheFileNameForTheTitle(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "plain.md", "---\ndescription: what it says\n---\nbody\n")
	got, err := lineFromFrontmatter(filepath.Join(dir, "plain.md"), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [plain](plain.md): what it says"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMemAddRefusals(t *testing.T) {
	t.Run("a flag it does not know", func(t *testing.T) {
		memEnv(t)
		var err error
		quiet(t, func() { err = memAdd([]string{"--bogus"}) })
		if err != exitError(2) {
			t.Errorf("got %v, want exit 2", err)
		}
	})
	t.Run("a name that is a path", func(t *testing.T) {
		memEnv(t)
		err := memAdd([]string{"a/b", "- [a/b](a/b.md): x"})
		if !isUsage(err) {
			t.Errorf("got %v, want a usage error", err)
		}
	})
	t.Run("an empty name", func(t *testing.T) {
		memEnv(t)
		if err := memAdd([]string{".md", "x"}); !isUsage(err) {
			t.Errorf("got %v, want a usage error", err)
		}
	})
}

func TestMemAddReportsEveryWayTheIndexCanFail(t *testing.T) {
	const line = "- [m](m.md): d"
	boom := errors.New("boom")

	t.Run("no index", func(t *testing.T) {
		_, index := memEnv(t)
		os.Remove(index)
		if err := memAdd([]string{"m", line}); err == nil || isUsage(err) {
			t.Errorf("got %v, want the read failure", err)
		}
	})
	t.Run("an index that cannot be written", func(t *testing.T) {
		cov43NeedChmod(t)
		_, index := memEnv(t)
		if err := os.Chmod(index, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := memAdd([]string{"m", line}); err == nil {
			t.Error("the write to a read-only index was reported as success")
		}
	})
	t.Run("an index that cannot be read back", func(t *testing.T) {
		_, index := memEnv(t)
		n := 0
		setVar(t, &readIndex, func(p string) ([]byte, error) {
			n++
			if n == 2 {
				return nil, boom
			}
			return os.ReadFile(p)
		})
		if err := memAdd([]string{"m", line}); err != boom {
			t.Errorf("got %v, want the re-read failure", err)
		}
		if !strings.Contains(indexText(t, index), line) {
			t.Error("the first write did not happen")
		}
	})
	t.Run("a line another writer overwrote", func(t *testing.T) {
		memEnv(t)
		n := 0
		setVar(t, &readIndex, func(p string) ([]byte, error) {
			n++
			if n == 2 {
				return []byte("# someone else's index\n"), nil
			}
			return os.ReadFile(p)
		})
		err := memAdd([]string{"m", line})
		if err == nil || !strings.Contains(err.Error(), "another writer overwrote it") {
			t.Errorf("got %v, want the overwritten refusal", err)
		}
		if rs, _ := loadRecords(); len(rs) != 0 {
			t.Error("an entry that is not in the index was recorded")
		}
	})
	t.Run("another session holds the index", func(t *testing.T) {
		_, index := memEnv(t)
		if _, err := store().Acquire("MEMORY.md", "other-2", "index edit", time.Hour); err != nil {
			t.Fatal(err)
		}
		err := memAdd([]string{"m", line})
		if err == nil || !strings.Contains(err.Error(), "being edited") {
			t.Errorf("got %v, want the being-edited refusal", err)
		}
		if strings.Contains(indexText(t, index), line) {
			t.Error("the index was edited without the lease")
		}
	})
	t.Run("records that cannot be read", func(t *testing.T) {
		memEnv(t)
		if err := os.MkdirAll(recordPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := memAdd([]string{"m", line}); err == nil {
			t.Error("a failure to read the records was dropped")
		}
	})
}

func TestMemAddTwiceRecordsOnce(t *testing.T) {
	memEnv(t)
	const line = "- [m](m.md): d"
	if err := memAdd([]string{"m", line}); err != nil {
		t.Fatal(err)
	}
	out := said(t, func() error { return memAdd([]string{"m", line}) })
	if !strings.Contains(out, "indexed m (already present)") {
		t.Errorf("second add said %q", out)
	}
	rs, err := loadRecords()
	if err != nil || len(rs) != 1 {
		t.Errorf("records = %v, %v; want exactly one", rs, err)
	}
}

func TestReachedViaDoesNotAnswerForAWalkThatCouldNotRead(t *testing.T) {
	cache := map[string]map[string]bool{}
	r := record{Name: "m", Line: "- [m](m.md): d", Index: filepath.Join(t.TempDir(), "gone", "MEMORY.md")}
	if reachedVia(r, cache) {
		t.Error("an unreadable directory reached a memory")
	}
	if len(cache) != 0 {
		t.Error("the failure was cached, so every later record would repeat it")
	}
	if lineReached(r, cache) {
		t.Error("lineReached answered yes for a walk that could not read")
	}
}

// verifyWith registers the records for this session and runs mem-verify.
func verifyWith(t *testing.T, rs []record, args ...string) (string, error) {
	t.Helper()
	if err := saveRecords(rs); err != nil {
		t.Fatal(err)
	}
	var err error
	out := said(t, func() error { err = memVerify(args); return err })
	return out, err
}

func TestMemVerifyEdges(t *testing.T) {
	t.Run("a flag it does not know", func(t *testing.T) {
		memEnv(t)
		var err error
		quiet(t, func() { err = memVerify([]string{"--bogus"}) })
		if err != exitError(2) {
			t.Errorf("got %v, want exit 2", err)
		}
	})
	t.Run("records that cannot be read", func(t *testing.T) {
		memEnv(t)
		if err := os.MkdirAll(recordPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := memVerify(nil); err == nil {
			t.Error("unreadable records were treated as none")
		}
	})
	t.Run("nothing recorded", func(t *testing.T) {
		memEnv(t)
		out := said(t, func() error { return memVerify(nil) })
		if !strings.Contains(out, "nothing recorded for this session") {
			t.Errorf("got %q", out)
		}
	})
	t.Run("an index that is gone", func(t *testing.T) {
		memEnv(t)
		gone := filepath.Join(t.TempDir(), "MEMORY.md")
		_, err := verifyWith(t, []record{{Name: "m", Line: "- [m](m.md): d", Index: gone}})
		if err == nil {
			t.Error("a missing index was not reported")
		}
	})
}

func TestMemVerifyDropsADanglingRowAndForgetsTheRecord(t *testing.T) {
	dir, index := memEnv(t)
	writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n- [g1](g1.md): d\n- [g2](g2.md): d\n- [m](m.md): t\n")
	writeMem(t, dir, "m.md", "body")
	rs := []record{
		{Name: "m", Line: "- [m](m.md): t", Index: index},
		{Name: "g1", Line: "- [g1](g1.md): d", Index: index},
		{Name: "g2", Line: "- [g2](g2.md): d", Index: index},
	}
	out, err := verifyWith(t, rs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"gone     g1 (dangling index row removed)",
		"gone     g2 (dangling index row removed)",
		"2 memories have been deleted; nothing was put back for them.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Every memory this session registered") {
		t.Error("said everything was reachable beside two deletions")
	}
	text := indexText(t, index)
	if strings.Contains(text, "g1.md") || strings.Contains(text, "g2.md") || !strings.Contains(text, "m.md") {
		t.Errorf("index after the repair:\n%s", text)
	}
	left, err := loadRecords()
	if err != nil || len(left) != 1 || left[0].Name != "m" {
		t.Errorf("records left = %v, %v; want only m", left, err)
	}
}

func TestMemVerifyDeletedMemoryIsNotPutBackAndSaysSoInTheSingular(t *testing.T) {
	_, index := memEnv(t)
	out, err := verifyWith(t, []record{{Name: "d", Line: "- [d](d.md): x", Index: index}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"gone     d (deleted, not put back)",
		"1 memory has been deleted; nothing was put back for it.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(indexText(t, index), "d.md") {
		t.Error("the row of a deleted memory was put back")
	}
	if left, _ := loadRecords(); len(left) != 0 {
		t.Errorf("records left = %v", left)
	}
}

func TestMemVerifyAcceptsARewordedRowAndRemembersTheNewWording(t *testing.T) {
	dir, index := memEnv(t)
	writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n- [a](a.md): new a\n- [b](b.md): new b\n")
	writeMem(t, dir, "a.md", "x")
	writeMem(t, dir, "b.md", "x")
	rs := []record{
		{Name: "a", Line: "- [a](a.md): old a", Index: index},
		{Name: "b", Line: "- [b](b.md): old b", Index: index},
	}
	out, err := verifyWith(t, rs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"changed  a", "changed  b", "2 index entries have been reworded"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	left, _ := loadRecords()
	if len(left) != 2 || left[0].Line != "- [a](a.md): new a" || left[1].Line != "- [b](b.md): new b" {
		t.Errorf("records after the run = %v", left)
	}
	again := said(t, func() error { return memVerify(nil) })
	if !strings.Contains(again, "ok       a") || strings.Contains(again, "changed") {
		t.Errorf("second run still reports the edit:\n%s", again)
	}

	// One entry says "entry has", not "entries have".
	if err := saveRecords(rs[:1]); err != nil {
		t.Fatal(err)
	}
	one := said(t, func() error { return memVerify(nil) })
	if !strings.Contains(one, "1 index entry has been reworded") {
		t.Errorf("singular wording lost:\n%s", one)
	}
}

func TestMemVerifyDanglingRowEdgesInTheLockedRewrite(t *testing.T) {
	setup := func(t *testing.T) (string, []record) {
		dir, index := memEnv(t)
		writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n- [g](g.md): d\n")
		return index, []record{{Name: "g", Line: "- [g](g.md): d", Index: index}}
	}
	t.Run("the index cannot be read under the lease", func(t *testing.T) {
		_, rs := setup(t)
		boom := errors.New("boom")
		setVar(t, &readIndex, func(string) ([]byte, error) { return nil, boom })
		if _, err := verifyWith(t, rs); err != boom {
			t.Errorf("got %v, want the read failure", err)
		}
	})
	t.Run("the row is already gone when the lease is held", func(t *testing.T) {
		index, rs := setup(t)
		setVar(t, &readIndex, func(string) ([]byte, error) { return []byte("# nothing links g\n"), nil })
		out, err := verifyWith(t, rs)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "gone     g (dangling index row removed)") {
			t.Errorf("output:\n%s", out)
		}
		if !strings.Contains(indexText(t, index), "g.md") {
			t.Error("a rewrite happened for a row that was not there")
		}
	})
	t.Run("another session holds the index", func(t *testing.T) {
		_, rs := setup(t)
		if _, err := store().Acquire("MEMORY.md", "other-2", "index edit", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyWith(t, rs); err == nil {
			t.Error("the row was dropped without the lease")
		}
	})
	t.Run("the records cannot be saved afterwards", func(t *testing.T) {
		_, rs := setup(t)
		if err := saveRecords(rs); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(recordPath()+".tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		quiet(t, func() { _ = capture(t, func() { err = memVerify(nil) }) })
		if err == nil {
			t.Error("a failed save of the records was dropped")
		}
	})
}

func TestMemVerifyMissingEntries(t *testing.T) {
	t.Run("a recorded line without a link is named, not re-inserted", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "m.md", "x")
		out, err := verifyWith(t, []record{{Name: "m", Line: "plain text", Index: index}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "MISSING  m (the recorded line has no link; add one with: agentsync mem-add m)") {
			t.Errorf("output:\n%s", out)
		}
		if strings.Contains(indexText(t, index), "plain text") {
			t.Error("a linkless line was put into the index")
		}
	})
	t.Run("--repair=false only reports", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "m.md", "x")
		out, err := verifyWith(t, []record{{Name: "m", Line: "- [m](m.md): d", Index: index}}, "--repair=false")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "MISSING  m\n") || !strings.Contains(out, "1 entry had been dropped") {
			t.Errorf("output:\n%s", out)
		}
		if strings.Contains(indexText(t, index), "m.md") {
			t.Error("--repair=false repaired")
		}
	})
	t.Run("the repair cannot read the index under the lease", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "m.md", "x")
		boom := errors.New("boom")
		setVar(t, &readIndex, func(string) ([]byte, error) { return nil, boom })
		if _, err := verifyWith(t, []record{{Name: "m", Line: "- [m](m.md): d", Index: index}}); err != boom {
			t.Errorf("got %v, want the read failure", err)
		}
	})
	t.Run("the repair is refused while another session edits", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "m.md", "x")
		if _, err := store().Acquire("MEMORY.md", "other-2", "index edit", time.Hour); err != nil {
			t.Fatal(err)
		}
		_, err := verifyWith(t, []record{{Name: "m", Line: "- [m](m.md): d", Index: index}})
		if err == nil || !strings.Contains(err.Error(), "being edited") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("two dropped entries say so in the plural", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "a.md", "x")
		writeMem(t, dir, "b.md", "x")
		out, err := verifyWith(t, []record{
			{Name: "a", Line: "- [a](a.md): d", Index: index},
			{Name: "b", Line: "- [b](b.md): d", Index: index},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"REPAIRED a", "REPAIRED b", "2 entries had been dropped"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})
}

func TestMemVerifyHubbedEntriesAreExplainedInTheSingularAndPlural(t *testing.T) {
	dir, index := memEnv(t)
	writeMem(t, dir, "MEMORY.md", "# i\n\n- [hub](hub.md): h\n")
	writeMem(t, dir, "hub.md", "- [a](a.md)\n- [b](b.md)\n")
	writeMem(t, dir, "a.md", "x")
	writeMem(t, dir, "b.md", "x")
	rs := []record{
		{Name: "a", Line: "- [a](a.md): d", Index: index},
		{Name: "b", Line: "- [b](b.md): d", Index: index},
	}
	out, err := verifyWith(t, rs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 entries are reached through a hub file") {
		t.Errorf("plural:\n%s", out)
	}
	if err := saveRecords(rs[:1]); err != nil {
		t.Fatal(err)
	}
	one := said(t, func() error { return memVerify(nil) })
	if !strings.Contains(one, "1 entry is reached through a hub file") {
		t.Errorf("singular:\n%s", one)
	}
}

func TestClaimArgumentRefusals(t *testing.T) {
	sandbox(t, "me-1111")
	var err error
	quiet(t, func() { err = claim([]string{"--bogus"}) })
	if err != exitError(2) {
		t.Errorf("a bad flag: got %v, want exit 2", err)
	}
	if err := claim(nil); err == nil || !strings.Contains(err.Error(), "usage: agentsync claim") {
		t.Errorf("no arguments: got %v", err)
	}
	quiet(t, func() { err = claim([]string{"."}) })
	if err == nil || !strings.Contains(err.Error(), "nothing to claim") {
		t.Errorf("only a directory: got %v", err)
	}
	errOut := quiet(t, func() { _ = capture(t, func() { err = claim([]string{"o/r", "."}) }) })
	if err != nil {
		t.Errorf("a resource beside a rejected one: %v", err)
	}
	if !strings.Contains(errOut, `ignored  "."`) {
		t.Errorf("the rejection was not named: %q", errOut)
	}
}

func TestClaimWaitOnAFreeResourceTakesIt(t *testing.T) {
	sandbox(t, "me-1111")
	out := said(t, func() error { return claim([]string{"--wait", "1s", "--every", "10ms", "o/r"}) })
	if !strings.Contains(out, "claimed  o/r") {
		t.Errorf("output:\n%s", out)
	}
	if l, _ := store().List(); len(l) != 1 || l[0].Owner != "me-1111" {
		t.Errorf("leases = %v", l)
	}
}

// ⛔ agentsync#33 variant c: a background `claim --wait` outliving its run.
// The wait must be DROPPED, and say so, once the session's transcript has been
// silent past the grace, instead of taking the lease for a session that is gone.
func TestClaimWaitIsDroppedWhenTheSessionHasEnded(t *testing.T) {
	const sid = "6ae1a34c-0000-4000-8000-000000000000"
	sandbox(t, sid)
	h := t.TempDir()
	fakeHome(t, h)
	dir := filepath.Join(h, ".claude", "projects", "-p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tr, old, old); err != nil {
		t.Fatal(err)
	}

	var err error
	out := said(t, func() error { err = claim([]string{"--wait", "1s", "o/r"}); return err })
	if err != errUnavailable {
		t.Errorf("got %v, want errUnavailable", err)
	}
	for _, want := range []string{"DROPPED  o/r", "the session that asked for this wait has ended"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if l, _ := store().List(); len(l) != 0 {
		t.Errorf("a lease was granted to a session that is gone: %v", l)
	}
}

func TestClaimReturnsAStoreFailureThatIsNotARefusal(t *testing.T) {
	cov43FileForStore(t, "me-1111")
	var err error
	quiet(t, func() { _ = capture(t, func() { err = claim([]string{"o/r"}) }) })
	if err == nil || err == errUnavailable {
		t.Errorf("got %v, want the store's own error", err)
	}
}

func TestShowMessagesCountsWhatItHidesAndSaysWhenItCannotRead(t *testing.T) {
	t.Run("an old message is counted, not shown", func(t *testing.T) {
		sandbox(t, "me-1111")
		past := store()
		past.Now = func() time.Time { return time.Now().Add(-3 * messageWindow) }
		if _, err := past.Tell("o/r", "them-2222", "ancient news"); err != nil {
			t.Fatal(err)
		}
		out := said(t, func() error { showMessages(store(), "o/r"); return nil })
		if !strings.Contains(out, "(1 older or unreadable message(s) not shown; agentsync messages o/r --max-age 0 for all)") {
			t.Errorf("output:\n%s", out)
		}
		if strings.Contains(out, "ancient news") || strings.Contains(out, "agentsync forget") {
			t.Errorf("a hidden message was shown:\n%s", out)
		}
	})
	t.Run("an unreadable inbox is said on stderr and stops there", func(t *testing.T) {
		cov43FileForStore(t, "me-1111")
		var out string
		errOut := quiet(t, func() { out = said(t, func() error { showMessages(store(), "o/r"); return nil }) })
		if !strings.Contains(errOut, "its messages could not be read") {
			t.Errorf("stderr = %q", errOut)
		}
		if out != "" {
			t.Errorf("stdout = %q, want nothing", out)
		}
	})
}

func TestReleaseEdges(t *testing.T) {
	sandbox(t, "me-1111")
	if err := release(nil); err == nil || !strings.Contains(err.Error(), "usage: agentsync release") {
		t.Errorf("no arguments: got %v", err)
	}
	// A store that has never been written has no directory to release in.
	if _, err := store().Acquire("x/y", "me-1111", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	var err error
	var out string
	errOut := quiet(t, func() { out = said(t, func() error { err = release([]string{".", "o/r"}); return err }) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, `ignored  "."`) {
		t.Errorf("rejection not named: %q", errOut)
	}
	if !strings.Contains(out, "not held  o/r") {
		t.Errorf("output: %q", out)
	}
}

func TestReleaseReturnsAStoreFailure(t *testing.T) {
	cov43FileForStore(t, "me-1111")
	if err := release([]string{"o/r"}); err == nil {
		t.Error("a store that cannot answer was reported as released")
	}
}

func TestClaimsFlagAndStoreFailures(t *testing.T) {
	var err error
	sandbox(t, "me-1111")
	quiet(t, func() { err = claims([]string{"--bogus"}) })
	if err != exitError(2) {
		t.Errorf("got %v, want exit 2", err)
	}
	cov43FileForStore(t, "me-1111")
	if err := claims(nil); err == nil {
		t.Error("an unreadable store was listed as empty")
	}
}

func TestClaimsHidesExpiredLeasesOnTheRecordAndShowsThemSorted(t *testing.T) {
	sandbox(t, "me-1111")
	past := store()
	past.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	for _, r := range []string{"b/r", "a/r"} {
		if _, err := past.Acquire(r, "them-2222", "note for "+r, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	out := said(t, func() error { return claims(nil) })
	for _, want := range []string{"2 expired lease(s) not shown; --all to see them.", "nothing is held right now."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "EXPIRED") {
		t.Errorf("an expired lease was listed without --all:\n%s", out)
	}

	all := said(t, func() error { return claims([]string{"--all"}) })
	if strings.Contains(all, "not shown") || strings.Contains(all, "nothing is held right now") {
		t.Errorf("--all still hides:\n%s", all)
	}
	a, b := strings.Index(all, "a/r"), strings.Index(all, "b/r")
	if a < 0 || b < 0 || a > b {
		t.Errorf("not sorted by resource:\n%s", all)
	}
	for _, want := range []string{"EXPIRED", `"note for a/r"`, `"note for b/r"`} {
		if !strings.Contains(all, want) {
			t.Errorf("--all lacks %q:\n%s", want, all)
		}
	}
}

func TestWhoamiNamesTheOwnerTheLocksAndTheIndex(t *testing.T) {
	sandbox(t, "me-1111")
	t.Setenv("AGENTSYNC_MEMORY", "/some/MEMORY.md")
	out := said(t, func() error { return whoami(nil) })
	for _, want := range []string{"owner   me-1111", "locks   " + filepath.Join(root(), "locks"), "index   /some/MEMORY.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestParseFlagsTurnsTheFlagPackageIntoExitStatuses(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := newFlags("x")
		fs.Bool("known", false, "")
		return fs
	}
	cases := []struct {
		args []string
		want error
	}{
		{[]string{"--known"}, nil},
		{[]string{"--bogus"}, exitError(2)},
		{[]string{"-h"}, exitError(0)},
		{[]string{"--help"}, exitError(0)},
	}
	for _, c := range cases {
		var got error
		quiet(t, func() { got = parseFlags(newSet(), c.args) })
		if got != c.want {
			t.Errorf("parseFlags(%v) = %v, want %v", c.args, got, c.want)
		}
	}
	if got := exitError(3).Error(); got != "exit status 3" {
		t.Errorf("Error() = %q", got)
	}
}

func TestEveryCommandAnswersABadFlagWithExit2(t *testing.T) {
	sandbox(t, "me-1111")
	for name, fn := range map[string]func([]string) error{
		"mem-fold": memFold, "gate": gate, "tell": tell, "messages": messages,
		"forget": forget, "claim": claim, "claims": claims, "mem-add": memAdd, "mem-verify": memVerify,
	} {
		var err error
		quiet(t, func() { err = fn([]string{"--bogus"}) })
		if err != exitError(2) {
			t.Errorf("%s --bogus: got %v, want exit 2", name, err)
		}
		quiet(t, func() { err = fn([]string{"-h"}) })
		if err != exitError(0) {
			t.Errorf("%s -h: got %v, want exit 0", name, err)
		}
	}
}

func TestRunMapsEveryOutcomeToAnExitStatus(t *testing.T) {
	sandbox(t, "me-1111")
	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(t.TempDir(), "MEMORY.md"))

	type result struct {
		code   int
		stdout string
		stderr string
	}
	do := func(argv ...string) result {
		var r result
		r.stderr = quiet(t, func() { r.stdout = capture(t, func() { r.code = run(append([]string{"agentsync"}, argv...)) }) })
		return r
	}

	if r := do(); r.code != 2 || !strings.Contains(r.stderr, "usage: agentsync {claim|") {
		t.Errorf("no command: %+v", r)
	}
	if r := do("frobnicate"); r.code != 2 || !strings.Contains(r.stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: %+v", r)
	}
	if r := do("whoami"); r.code != 0 || !strings.Contains(r.stdout, "owner   me-1111") || r.stderr != "" {
		t.Errorf("success: %+v", r)
	}
	if r := do("claims", "--bogus"); r.code != 2 {
		t.Errorf("exit error: %+v", r)
	}
	if r := do("claims", "-h"); r.code != 0 {
		t.Errorf("help: %+v", r)
	}
	if r := do("mem-add"); r.code != 2 || !strings.Contains(r.stderr, "agentsync: usage: agentsync mem-add") {
		t.Errorf("usage error: %+v", r)
	}
	if r := do("claim", "o/r"); r.code != 0 {
		t.Fatalf("first claim: %+v", r)
	}
	t.Setenv("AGENTSYNC_OWNER", "other-2222")
	if r := do("claim", "o/r"); r.code != 1 || r.stderr != "" || !strings.Contains(r.stdout, "REFUSED  o/r") {
		t.Errorf("a refusal is a result, not a crash: %+v", r)
	}
	cov43FileForStore(t, "me-1111")
	if r := do("claim", "o/r"); r.code != 1 || !strings.HasPrefix(r.stderr, "agentsync: ") {
		t.Errorf("a crash: %+v", r)
	}
}

func TestMainExitsWithWhatRunReturns(t *testing.T) {
	sandbox(t, "me-1111")
	t.Setenv("AGENTSYNC_MEMORY", "/x/MEMORY.md")
	code := -1
	setVar(t, &osExit, func(c int) { code = c })
	setVar(t, &os.Args, []string{"agentsync", "whoami"})
	capture(t, main)
	if code != 0 {
		t.Errorf("exit status %d, want 0", code)
	}
	os.Args = []string{"agentsync"}
	quiet(t, main)
	if code != 2 {
		t.Errorf("exit status %d with no command, want 2", code)
	}
}
