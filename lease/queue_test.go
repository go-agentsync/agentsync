package lease

import (
	"errors"
	"os"
	"path/filepath"
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
	if err := os.Mkdir(s.queuePath("o/r")+".tmp", 0o755); err != nil { // writing a directory fails
		t.Fatal(err)
	}
	if _, err := s.Wait("o/r", "bob", t0.Add(time.Hour)); err == nil {
		t.Error("Wait reported a place it never wrote")
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

func TestQueuedSaysWhoIsAhead(t *testing.T) {
	e := &Queued{Resource: "o/r", Ahead: []Waiter{{Owner: "bob"}, {Owner: "carol"}}}
	if got, want := e.Error(), "o/r is free but 2 session(s) waited longer: bob, carol"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
