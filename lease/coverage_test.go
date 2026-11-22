package lease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Each test here makes one error path real. They are written against the
// filesystem wherever it can be made to fail (a directory where a file belongs, a
// name that is a file, a dangling link) and against the three seams in lease.go
// and message.go where it cannot.

// year10000 cannot be marshalled to JSON, so anything that records it fails.
var year10000 = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

func TestHeldNamesTheHolderAndTheDeadline(t *testing.T) {
	h := &Held{By: Lease{Owner: "alice", Expires: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	if got, want := h.Error(), "held by alice until 2026-01-02T03:04:05Z"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAnUnparseableLeaseElsewhereIsNotAHolder(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if err := os.WriteFile(filepath.Join(s.Dir, "torn.lease"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatalf("a torn neighbour blocked the claim: %v", err)
	}
}

func TestALongResourceNameIsCutToAFileNameThatFits(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	name := "o/" + strings.Repeat("x", 300)
	if got := filepath.Base(s.path(name)); len(got) != 120+len(".lease") {
		t.Errorf("file name has %d bytes, want %d", len(got), 120+len(".lease"))
	}
	if _, err := s.Acquire(name, "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(name, "bob", "", time.Hour); !isHeld(err) {
		t.Errorf("the cut name stopped identifying the lease: %v", err)
	}
}

func TestALeaseThatCannotBeEncodedIsRefusedNotWritten(t *testing.T) {
	s := store(t, year10000)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err == nil {
		t.Fatal("acquired a lease whose deadline cannot be recorded")
	}
	left, _ := filepath.Glob(filepath.Join(s.Dir, "*"))
	if len(left) != 0 {
		t.Errorf("a refused claim left %v behind", left)
	}
}

func TestADirectoryOneCannotCreateInIsReportedNotReadAsHeld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	s := store(t, time.Unix(1000, 0))
	if err := os.Chmod(s.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.Dir, 0o755) })
	_, err := s.Acquire("o/r", "alice", "", time.Hour)
	if err == nil || isHeld(err) {
		t.Fatalf("got %v, want the filesystem's own error", err)
	}
}

// A dangling link is a name that exists (the link refuses it) and a file that
// does not (the read finds nothing): the one state in which the create and the
// take-over each conclude "try again", twice.
func TestAClaimThatLosesTheRaceTwiceSaysSo(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if err := os.Symlink(filepath.Join(s.Dir, "nowhere"), s.path("o/r")); err != nil {
		t.Skip("no symlinks here")
	}
	_, err := s.Acquire("o/r", "alice", "", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "lost the race twice") {
		t.Fatalf("got %v, want the lost-race error", err)
	}
}

func TestACreateReportsAFailedWriteOrCloseAndLeavesNothing(t *testing.T) {
	boom := errors.New("boom")
	dir := t.TempDir()
	p := filepath.Join(dir, "x.lease")

	oldW := tmpWrite
	tmpWrite = func(*os.File, []byte) error { return boom }
	err := createExclusive(p, []byte("body"))
	tmpWrite = oldW
	if !errors.Is(err, boom) {
		t.Errorf("write failure: got %v", err)
	}

	oldC := tmpClose
	tmpClose = func(f *os.File) error { f.Close(); return boom }
	err = createExclusive(p, []byte("body"))
	tmpClose = oldC
	if !errors.Is(err, boom) {
		t.Errorf("close failure: got %v", err)
	}

	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 0 {
		t.Errorf("a failed create left %v", left)
	}
}

func TestWriteReportsEveryWayItCanFail(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	p := s.path("o/r")

	// The record cannot be encoded.
	if _, err := s.write(p, Lease{Resource: "o/r", Owner: "a", Expires: year10000}); err == nil {
		t.Error("encoded a deadline in year 10000")
	}
	// The temporary's name is taken by a directory.
	if err := os.Mkdir(p+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write(p, Lease{Resource: "o/r", Owner: "a"}); err == nil {
		t.Error("wrote through a directory")
	}
	if err := os.Remove(p + ".tmp"); err != nil {
		t.Fatal(err)
	}
	// The rename into place fails.
	boom := errors.New("boom")
	old := renameFile
	renameFile = func(string, string) error { return boom }
	defer func() { renameFile = old }()
	if _, err := s.write(p, Lease{Resource: "o/r", Owner: "a"}); !errors.Is(err, boom) {
		t.Errorf("rename failure: got %v", err)
	}
}

func TestARecordWithNoResourceOrOwnerIsNotALease(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	p := s.path("o/r")
	for _, body := range []string{`{}`, `{"resource":"o/r"}`, `{"owner":"alice"}`} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.read(p); err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("%s: got %v, want the incomplete-record error", body, err)
		}
	}
}

func TestReleaseReportsAnUnreadableLeaseAndAFailedRemoval(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	p := s.path("o/r")
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if released, err := s.Release("o/r", "alice"); err == nil || released {
		t.Errorf("torn lease: released=%v err=%v, want an error", released, err)
	}

	if _, err := s.Acquire("o/s", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	s.remove = func(string) error { return boom }
	released, err := s.Release("o/s", "alice")
	if !errors.Is(err, boom) || released {
		t.Errorf("failed removal: released=%v err=%v", released, err)
	}
	if _, err := os.Stat(s.path("o/s")); err != nil {
		t.Errorf("a release that failed must leave the lease: %v", err)
	}
}

func TestListingAndCoveringSkipWhatIsNotALease(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir, "d.lease"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.List(); err != nil || len(got) != 1 {
		t.Errorf("List = %v, %v; want the one lease", got, err)
	}
	if got, err := s.Covering("o/r"); err != nil || len(got) != 1 {
		t.Errorf("Covering = %v, %v; want the one lease", got, err)
	}
}

func TestListReportsADirectoryThatIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: f}
	if _, err := s.List(); err == nil {
		t.Error("List over a file reported nothing wrong")
	}
}

func TestLockReturnsAnUnlockThatIsSafeToCallOnEveryFailure(t *testing.T) {
	// The directory is not there: the lock cannot be created.
	s := Store{Dir: filepath.Join(t.TempDir(), "absent")}
	unlock, err := s.lock(filepath.Join(s.Dir, "x.lease"))
	if err == nil {
		t.Fatal("locked in a directory that does not exist")
	}
	unlock()

	// The lock is held and fresh: the wait gives up.
	shortLocks(t)
	s = store(t, time.Unix(1000, 0))
	p := s.path("o/r")
	unlock, err = s.lock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	unlock2, err := s.lock(p)
	if err == nil {
		t.Fatal("took a lock that is held")
	}
	unlock2()
	if _, serr := os.Stat(p + ".lock"); serr != nil {
		t.Errorf("giving up removed the other holder's lock: %v", serr)
	}
}

func TestATakesLogWhoseLastLineHasNoNewlineIsStillRead(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	line := `{"resource":"o/r","from":"a","to":"b","idle_for":"1m0s"}`
	if err := os.WriteFile(s.takesPath(), []byte(line+"\n\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Takes()
	if err != nil || len(got) != 2 {
		t.Fatalf("Takes = %v, %v; want both lines", got, err)
	}
}

func TestTakesReportsALogItCannotRead(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if err := os.Mkdir(s.takesPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takes(); err == nil {
		t.Error("a log that is a directory read as empty")
	}
}

func TestTouchRefusesToRecordWhatItCannotEncode(t *testing.T) {
	base := time.Date(9999, 12, 31, 12, 0, 0, 0, time.UTC)
	s := store(t, base)
	if _, err := s.Acquire("o/r", "alice", "", 11*time.Hour); err != nil {
		t.Fatal(err)
	}
	// The same instant (so the lease has not expired) written in a zone where
	// the calendar year is already 10000.
	s.Now = func() time.Time {
		return time.Date(10000, 1, 1, 0, 0, 0, 0, time.FixedZone("far", 14*3600))
	}
	if ok, err := s.Touch("o/r", "alice"); err == nil || ok {
		t.Errorf("Touch = %v, %v; want the encoding error", ok, err)
	}
}

func TestReleaseUnlessKeptReportsAFailedRemoval(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	s.remove = func(string) error { return boom }
	if released, _, err := s.ReleaseUnlessKept("o/r", "alice"); !errors.Is(err, boom) || released {
		t.Errorf("released=%v err=%v", released, err)
	}
}

func TestTellReportsEachWayItCanFail(t *testing.T) {
	// Directory cannot be made: its parent is a file.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Dir: filepath.Join(f, "sub")}).Tell("o/r", "a", "hi"); err == nil {
		t.Error("told into a directory that cannot exist")
	}

	// The message cannot be encoded.
	s := Store{Dir: t.TempDir(), Now: at(year10000)}
	if _, err := s.Tell("o/r", "a", "hi"); err == nil {
		t.Error("stored a message stamped year 10000")
	}

	// The messages file is a directory.
	s = Store{Dir: t.TempDir(), Now: at(time.Unix(1000, 0))}
	if err := os.Mkdir(s.messagesPath("o/r"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tell("o/r", "a", "hi"); err == nil {
		t.Error("told into a directory")
	}

	// The write itself fails.
	s = Store{Dir: t.TempDir(), Now: at(time.Unix(1000, 0))}
	boom := errors.New("boom")
	old := appendLine
	appendLine = func(*os.File, []byte) error { return boom }
	defer func() { appendLine = old }()
	if _, err := s.Tell("o/r", "a", "hi"); !errors.Is(err, boom) {
		t.Errorf("write failure: got %v", err)
	}
}

func TestMessagesReportsWhatItCannotReadAndSkipsBlankLines(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Store{Dir: filepath.Join(f, "sub")}).Messages("o/r", 0); err == nil {
		t.Error("read from a directory that cannot exist")
	}

	s := Store{Dir: t.TempDir(), Now: at(time.Unix(1000, 0))}
	line := `{"resource":"o/r","from":"a","text":"hi","at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(s.messagesPath("o/r"), []byte("\n   \n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, dropped, err := s.Messages("o/r", 0)
	if err != nil || len(live) != 1 || dropped != 0 {
		t.Errorf("got %v, dropped %d, %v; blank lines are neither messages nor losses", live, dropped, err)
	}

	// One line longer than the reader's limit is an error, not a truncated message.
	if err := os.WriteFile(s.messagesPath("o/r"), []byte(strings.Repeat("x", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Messages("o/r", 0); err == nil {
		t.Error("a line over the limit was returned as a message")
	}
}

func TestListAndCoveringOnAbsentUnreadableAndTornDirectories(t *testing.T) {
	absent := Store{Dir: filepath.Join(t.TempDir(), "absent")}
	if got, err := absent.List(); err != nil || got != nil {
		t.Errorf("List of an absent directory = %v, %v; want nothing, no error", got, err)
	}
	if _, err := absent.Covering("o/r"); err == nil {
		t.Error("Covering read an absent directory as 'nobody holds it'")
	}

	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "torn.lease"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.List(); err != nil || len(got) != 1 {
		t.Errorf("List = %v, %v; a torn file must be skipped, not fatal", got, err)
	}
	if _, err := s.Covering("o/r"); err == nil || !strings.Contains(err.Error(), "torn.lease is unreadable") {
		t.Errorf("Covering over a torn file = %v; it may be the holder, so it must not answer", err)
	}
}

func TestALeaseWithoutAnActiveStampWasLastActiveWhenAcquired(t *testing.T) {
	acq := time.Unix(500, 0)
	if got := (Lease{Acquired: acq}).LastActive(); !got.Equal(acq) {
		t.Errorf("LastActive = %v, want %v", got, acq)
	}
}

func TestAStoreWithoutAClockUsesTheWallClock(t *testing.T) {
	before := time.Now()
	if got := (Store{}).now(); got.Before(before) || time.Since(got) > time.Minute {
		t.Errorf("now() = %v, not the wall clock", got)
	}
}
