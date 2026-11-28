package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// clocked is a store whose clock a test moves by hand.
func clocked(t *testing.T, at time.Time) (Store, *time.Time) {
	t.Helper()
	now := at
	return Store{Dir: t.TempDir(), Now: func() time.Time { return now }}, &now
}

func mustWait(t *testing.T, s Store, resource, owner string, until time.Time) int {
	t.Helper()
	n, err := s.Wait(resource, owner, until)
	if err != nil {
		t.Fatalf("%s could not take a place in line: %v", owner, err)
	}
	return n
}

func wantQueued(t *testing.T, err error, ahead ...string) {
	t.Helper()
	var q *Queued
	if !errors.As(err, &q) {
		t.Fatalf("got %v, want *Queued behind %v", err, ahead)
	}
	if len(q.Ahead) != len(ahead) {
		t.Fatalf("queued behind %v, want %v", q.Ahead, ahead)
	}
	for i, o := range ahead {
		if q.Ahead[i].Owner != o {
			t.Errorf("place %d is %q, want %q", i, q.Ahead[i].Owner, o)
		}
	}
}

// ⛔ The defect this file exists for: the lease lapses, and whoever asks first
// gets it. Here the session that has waited longest must get it, however late
// it asks.
func TestTheLongestWaiterGetsAFreedLease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	if _, err := s.Acquire("o/r", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	*now = t0.Add(time.Second)
	if n := mustWait(t, s, "o/r", "carol", t0.Add(time.Hour)); n != 1 {
		t.Errorf("carol has %d ahead, want 1 (bob)", n)
	}

	*now = t0.Add(2 * time.Minute) // alice's lease has lapsed
	_, err := s.Acquire("o/r", "carol", "", time.Minute)
	wantQueued(t, err, "bob")

	got, err := s.Acquire("o/r", "bob", "", time.Minute)
	if err != nil || got.Owner != "bob" {
		t.Fatalf("bob, first in line, got %+v, %v", got, err)
	}
	// Winning takes bob out of the line: carol is next, with nobody ahead.
	q, err := s.Queue("o/r")
	if err != nil || len(q) != 1 || q[0].Owner != "carol" {
		t.Errorf("line after bob won = %+v, %v; want only carol", q, err)
	}
}

// ⛔ The line binds a session that never waited too. Otherwise any plain
// `claim` arriving at the right moment jumps it, which is the lottery again.
func TestAPlainClaimDoesNotJumpTheLine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	_, err := s.Acquire("o/r", "dave", "", time.Minute)
	wantQueued(t, err, "bob")
}

// A live lease is still reported as Held, with its holder: "alice has it" is
// more useful than "bob is in line", and it is what the caller already prints.
func TestALiveHolderIsStillReportedAsHeld(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	_, err := s.Acquire("o/r", "carol", "", time.Minute)
	var held *Held
	if !errors.As(err, &held) || held.By.Owner != "alice" {
		t.Fatalf("got %v, want Held by alice", err)
	}
}

// Extending one's own lease takes nothing from anybody in line.
func TestAHolderExtendingItsOwnLeaseJumpsNobody(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	if _, err := s.Acquire("o/r", "alice", "renewed", time.Hour); err != nil {
		t.Fatalf("alice could not extend her own lease with bob in line: %v", err)
	}
}

// An idle holder's lease goes to the head of the line, not to whoever notices.
func TestAnIdleLeaseGoesToTheHeadOfTheLine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	s.Idle = 15 * time.Minute
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "o/r", "bob", t0.Add(2*time.Hour))
	*now = t0.Add(16 * time.Minute)
	_, err := s.Acquire("o/r", "carol", "", time.Minute)
	wantQueued(t, err, "bob")
	if got, err := s.Acquire("o/r", "bob", "", time.Minute); err != nil || got.Owner != "bob" {
		t.Fatalf("bob got %+v, %v", got, err)
	}
}

// ⛔ A place keeps its ORIGINAL time however often it is refreshed: the
// refresh is what a waiting loop does every 15 s, and if it moved the waiter
// to the back the longest waiter would always be last.
func TestRefreshingKeepsTheOriginalPlace(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Minute))
	*now = t0.Add(10 * time.Second)
	mustWait(t, s, "o/r", "carol", t0.Add(time.Minute))
	*now = t0.Add(20 * time.Second)
	if n := mustWait(t, s, "o/r", "bob", t0.Add(2*time.Minute)); n != 0 {
		t.Errorf("bob refreshed and has %d ahead, want 0", n)
	}
	q, _ := s.Queue("o/r")
	if len(q) != 2 || q[0].Owner != "bob" || !q[0].Since.Equal(t0) {
		t.Errorf("line = %+v; want bob first, since %s", q, t0)
	}
}

// ⛔ A waiter that died must not hold the line for ever: that is the outage
// the lease's own expiry exists to prevent, moved to the queue.
func TestALapsedPlaceHoldsNobody(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Minute))
	*now = t0.Add(time.Minute) // the last instant bob's place holds
	_, err := s.Acquire("o/r", "dave", "", time.Minute)
	wantQueued(t, err, "bob")
	*now = t0.Add(time.Minute + time.Second)
	if _, err := s.Acquire("o/r", "dave", "", time.Minute); err != nil {
		t.Fatalf("bob's place lapsed and dave was still refused: %v", err)
	}
}

func TestLeavingFreesThePlace(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	if err := s.Leave("o/r", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave("o/r", "nobody"); err != nil {
		t.Errorf("leaving a line one is not in: %v", err)
	}
	if _, err := s.Acquire("o/r", "dave", "", time.Minute); err != nil {
		t.Fatalf("bob left and dave was still refused: %v", err)
	}
	if _, err := os.Stat(s.queuePath("o/r")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty line left its file behind: %v", err)
	}
}

// Every spelling of one repository is one line, as it is one lease.
func TestEverySpellingOfARepositoryIsOneLine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "github.com/O/R", "bob", t0.Add(time.Hour))
	_, err := s.Acquire("o/r", "dave", "", time.Minute)
	wantQueued(t, err, "bob")
}

// A torn queue file must not wedge the resource.
func TestACorruptQueueDoesNotWedge(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if err := os.WriteFile(s.queuePath("o/r"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("o/r", "dave", "", time.Minute); err != nil {
		t.Fatalf("a corrupt queue blocked the claim: %v", err)
	}
	if n := mustWait(t, s, "o/r", "bob", t0.Add(time.Hour)); n != 0 {
		t.Errorf("bob has %d ahead in a line rebuilt from a corrupt file", n)
	}
}

// The queue file must not read as a lease: List and the gates count *.lease.
func TestTheQueueIsNotListedAsALease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	ls, err := s.List()
	if err != nil || len(ls) != 0 {
		t.Errorf("List = %+v, %v; want no lease", ls, err)
	}
}

// ⛔ A queue that cannot be READ is not an empty one. Taking it for empty would
// hand the lease to whoever asks first -- the lottery -- while every waiter
// believes it is in line. Only a MISSING or TORN file is empty.
func TestAQueueThatCannotBeReadIsAnErrorNotAnEmptyLine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if err := os.Mkdir(s.queuePath("o/r"), 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	if _, err := s.Queue("o/r"); err == nil {
		t.Error("Queue read a directory as an empty line")
	}
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait took a place in a line it could not read")
	}
	if err := s.Leave("o/r", "bob"); err == nil {
		t.Error("Leave reported success on a line it could not read")
	}
	if _, err := s.Acquire("o/r", "dave", "", time.Minute); err == nil {
		t.Error("Acquire granted a lease past a line it could not read")
	}
}

func TestAStoreThatCannotBeCreatedRefusesAPlace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: filepath.Join(file, "locks"), Now: func() time.Time { return time.Unix(1000, 0) }}
	if _, err := s.Wait("o/r", "bob", time.Unix(2000, 0)); err == nil {
		t.Error("Wait succeeded in a store directory that cannot exist")
	}
	if err := s.Leave("o/r", "bob"); err == nil {
		t.Error("Leave succeeded in a store directory that cannot exist")
	}
}

func TestAQueueThatCannotBeLockedIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if err := os.Chmod(s.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.Dir, 0o755) })
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait took a place without the queue's lock")
	}
	if err := s.Leave("o/r", "bob"); err == nil {
		t.Error("Leave changed the line without the queue's lock")
	}
}

// ⛔ The lock alone, with a writable directory: a Wait that went on without it
// would race another waiter's read-modify-write and drop one of the two places.
func TestALiveQueueLockIsWaitedOnNotIgnored(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	saved := lockTries
	lockTries = 3
	t.Cleanup(func() { lockTries = saved })
	if err := os.WriteFile(s.queuePath("o/r")+".lock", []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait wrote its place while another session held the queue's lock")
	}
}

// The read alone fails, the write would succeed: a Wait must not rebuild the
// line from nothing and drop everyone already in it.
func TestAnUnreadableQueueIsNotOverwritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if err := os.WriteFile(s.queuePath("o/r"), []byte("[]"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait replaced a line it could not read")
	}
}

func TestAPlaceThatCannotBeWrittenIsAnError(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	s.rename = func(string, string) error { return errors.New("injected") }
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait reported a place it never wrote")
	}
	// And the private temporary file is not left behind.
	if left, _ := filepath.Glob(filepath.Join(s.Dir, ".queue-*.tmp")); len(left) != 0 {
		t.Errorf("a failed write left %v", left)
	}
}

func TestATemporaryFileThatCannotBeMadeIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	s, _ := clocked(t, time.Unix(1000, 0))
	if err := os.Chmod(s.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.Dir, 0o755) })
	if err := s.writeQueue(s.queuePath("o/r"), []Waiter{{Owner: "bob"}}); err == nil {
		t.Error("writeQueue succeeded in a directory it cannot write")
	}
}

// ⛔ Review of #53, CONFIRMED: the line was keyed on the exact file, while the
// lease conflict spans the dashed, bare and organisation spellings -- so
// `claim gitsafe` walked past a line kept under go-gitsafe/gitsafe. The line
// must bind every spelling the lease does.
func TestNoSpellingWalksPastTheLine(t *testing.T) {
	for _, other := range []string{"go-gitsafe-gitsafe", "gitsafe", "go-gitsafe", "github.com/go-gitsafe/gitsafe"} {
		t.Run(other, func(t *testing.T) {
			t0 := time.Unix(1000, 0)
			s, _ := clocked(t, t0)
			mustWait(t, s, "go-gitsafe/gitsafe", "bob", t0.Add(time.Hour))
			_, err := s.Acquire(other, "carol", "", time.Minute)
			wantQueued(t, err, "bob")
			// And the other way round: a line under the other spelling binds
			// the canonical name.
			s2, _ := clocked(t, t0)
			mustWait(t, s2, other, "bob", t0.Add(time.Hour))
			_, err = s2.Acquire("go-gitsafe/gitsafe", "carol", "", time.Minute)
			wantQueued(t, err, "bob")
		})
	}
}

// Two lines that are one claim merge by arrival, and a session waiting under
// two spellings is one place, its earliest.
func TestLinesUnderTwoSpellingsMergeByArrival(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "gitsafe", "bob", t0.Add(time.Hour))
	*now = t0.Add(time.Second)
	mustWait(t, s, "go-gitsafe/gitsafe", "alice", t0.Add(time.Hour))
	*now = t0.Add(2 * time.Second)
	mustWait(t, s, "go-gitsafe/gitsafe", "bob", t0.Add(time.Hour))
	if n := mustWait(t, s, "go-gitsafe/gitsafe", "carol", t0.Add(time.Hour)); n != 2 {
		t.Errorf("carol has %d ahead, want 2 (bob once, alice)", n)
	}
	_, err := s.Acquire("go-gitsafe/gitsafe", "carol", "", time.Minute)
	wantQueued(t, err, "bob", "alice")
}

// An unrelated line, or one whose waiters predate the Resource field, binds
// nothing outside its own file.
func TestAnUnrelatedLineBindsNothing(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "go-other/thing", "bob", t0.Add(time.Hour))
	if err := os.WriteFile(filepath.Join(s.Dir, "legacy.queue"),
		[]byte(`[{"owner":"old","since":"1970-01-01T00:16:40Z","until":"2100-01-01T00:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-gitsafe/gitsafe", "carol", "", time.Minute); err != nil {
		t.Fatalf("an unrelated line refused carol: %v", err)
	}
	if got := s.Lines(); len(got) != 1 || len(got["go-other/thing"]) != 1 {
		t.Errorf("Lines = %+v; want only go-other/thing (the legacy line names no resource)", got)
	}
}

// ⛔ Review of #53, CONFIRMED: "extending one's own lease" passed whatever the
// lease's state, so a holder whose lease had EXPIRED got it back ahead of the
// line -- the lost hand-over this queue exists to stop.
func TestAnExpiredHolderQueuesLikeAnyone(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	if _, err := s.Acquire("o/r", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "o/r", "bob", t0.Add(2*time.Hour))
	*now = t0.Add(time.Hour)
	_, err := s.Acquire("o/r", "alice", "", time.Minute)
	wantQueued(t, err, "bob")
}

// ⛔ Review of #53, CONFIRMED with a stolen lock: concurrent waiters each keep
// their place. With a fixed temporary name, writers the lock failed to
// separate renamed each other's file away and 24 of 24 Waits failed.
func TestConcurrentWaitersAllKeepTheirPlace(t *testing.T) {
	saved := lockStale
	lockStale = 0 // every lock looks stale: the lock no longer separates them
	t.Cleanup(func() { lockStale = saved })
	s := Store{Dir: t.TempDir()}
	until := time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Wait("o/r", fmt.Sprintf("w%02d", i), until); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent Wait failed: %v", err)
	}
}

func TestALineThatCannotBeEmptiedIsAnError(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	mustWait(t, s, "o/r", "bob", t0.Add(time.Hour))
	s.remove = func(string) error { return errors.New("injected") }
	if err := s.Leave("o/r", "bob"); err == nil {
		t.Error("Leave reported bob gone while his place is still on disk")
	}
}

// Only a *.queue file is a line: a stray JSON file that happens to hold
// waiter-shaped records for the same resource binds nobody.
func TestOnlyAQueueFileIsALine(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	stray := `[{"resource":"o/r","owner":"ghost","since":"1970-01-01T00:16:40Z","until":"2100-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(s.Dir, "o_r.json"), []byte(stray), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("o/r", "carol", "", time.Minute); err != nil {
		t.Fatalf("a stray file refused carol: %v", err)
	}
}

// ⛔ Review of #53, CONFIRMED: a session that waited under one spelling and won
// under another left only the second line; its old place then held the next
// waiter back after it had released.
func TestWinningLeavesTheLineUnderEverySpelling(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "go-gitsafe/gitsafe", "bob", t0.Add(time.Hour))
	mustWait(t, s, "go-other/thing", "bob", t0.Add(time.Hour)) // unrelated: kept
	if _, err := s.Acquire("gitsafe", "bob", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	*now = t0.Add(time.Second)
	mustWait(t, s, "go-gitsafe/gitsafe", "carol", t0.Add(time.Hour))
	if _, err := s.Release("gitsafe", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-gitsafe/gitsafe", "carol", "", time.Minute); err != nil {
		t.Fatalf("bob won and released, and carol is still behind his old place: %v", err)
	}
	if q, _ := s.Queue("go-other/thing"); len(q) != 1 {
		t.Errorf("winning gitsafe took bob out of an unrelated line: %+v", q)
	}
}

func TestLeavingAnOwnLineThatCannotBeReadIsAnError(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if err := os.Mkdir(s.queuePath("o/r"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave("o/r", "bob"); err == nil {
		t.Error("Leave reported success on its own line it could not read")
	}
}

// A place found without its resource name (written by an earlier build of
// this change) gets it back on refresh, so other spellings see it again.
func TestARefreshGivesAPlaceBackItsName(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	legacy := `[{"owner":"bob","since":"1970-01-01T00:16:40Z","until":"2100-01-01T00:00:00Z"}]`
	if err := os.WriteFile(s.queuePath("go-gitsafe/gitsafe"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "go-gitsafe/gitsafe", "bob", t0.Add(time.Hour))
	_, err := s.Acquire("gitsafe", "carol", "", time.Minute)
	wantQueued(t, err, "bob")
}

// Line is what `claims` prints: every spelling's waiters, each session once.
func TestLineMergesEverySpelling(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "gitsafe", "bob", t0.Add(time.Hour))
	*now = t0.Add(time.Second)
	mustWait(t, s, "go-gitsafe/gitsafe", "alice", t0.Add(time.Hour))
	q, err := s.Line("go-gitsafe/gitsafe")
	if err != nil || len(q) != 2 || q[0].Owner != "bob" || q[1].Owner != "alice" {
		t.Errorf("Line = %+v, %v; want bob then alice", q, err)
	}
	if err := os.Mkdir(s.queuePath("o/r"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Line("o/r"); err == nil {
		t.Error("Line read a directory as an empty line")
	}
}

// A waiter for a repository the caller itself holds cannot be granted it, so it
// does not stop the caller widening its own claim to the organisation.
func TestAWaiterForWhatTheCallerHoldsDoesNotBlockIt(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if _, err := s.Acquire("go-x/a", "carol", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "go-x/a", "bob", t0.Add(time.Hour))
	if l, err := s.Acquire("go-x", "carol", "", time.Hour); err != nil || l.Owner != "carol" {
		t.Fatalf("carol, widening her own claim, got %+v, %v", l, err)
	}
}

// ⛔ Review of #53, round 3, CONFIRMED: bob waits for go-gitsafe/gitsafe only
// because carol holds `gitsafe`; carol re-spelling her own claim was QUEUED.
func TestAHolderIsNotQueuedBehindAWaiterOnlyItBlocks(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, _ := clocked(t, t0)
	if _, err := s.Acquire("gitsafe", "carol", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	mustWait(t, s, "go-gitsafe/gitsafe", "bob", t0.Add(time.Hour))
	if l, err := s.Acquire("go-gitsafe/gitsafe", "carol", "", time.Hour); err != nil || l.Owner != "carol" {
		t.Fatalf("carol, re-spelling her own claim, got %+v, %v", l, err)
	}
}

// ⛔ Review of #53, round 3, CONFIRMED: claiming go-x/a dropped the session's
// place for the organisation go-x, which go-x/a does not give it.
func TestClaimingARepositoryKeepsAPlaceForItsOrganisation(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s, now := clocked(t, t0)
	mustWait(t, s, "go-x", "bob", t0.Add(time.Hour))
	*now = t0.Add(time.Second)
	mustWait(t, s, "go-x/a", "bob", t0.Add(time.Hour))
	if _, err := s.Acquire("go-x/a", "bob", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if q, err := s.Queue("go-x"); err != nil || len(q) != 1 || q[0].Owner != "bob" {
		t.Errorf("Queue(go-x) after bob got go-x/a = %+v, %v; want bob kept", q, err)
	}
	if q, err := s.Queue("go-x/a"); err != nil || len(q) != 0 {
		t.Errorf("Queue(go-x/a) after bob got it = %+v, %v; want empty", q, err)
	}
}

func TestOverlapsIsOneClaim(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"go-x", "go-x/a", true},
		{"gitsafe", "go-gitsafe/gitsafe", true},
		{"go-x/a", "go-x/b", false},
	} {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAStoreWithNoDirectoryHasNoLines(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "never-made")}
	if got := s.Lines(); len(got) != 0 {
		t.Errorf("Lines of a store that does not exist = %+v", got)
	}
}

func TestQueuedSaysWhoIsAhead(t *testing.T) {
	e := &Queued{Resource: "o/r", Ahead: []Waiter{{Owner: "bob"}, {Owner: "carol"}}}
	if got, want := e.Error(), "o/r is free but 2 session(s) waited longer: bob, carol"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
