package lease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const idleAfter = 15 * time.Minute

func idleStore(t *testing.T, now *time.Time) Store {
	t.Helper()
	return Store{Dir: t.TempDir(), Now: func() time.Time { return *now }, Idle: idleAfter}
}

func isHeld(err error) bool { var h *Held; return errors.As(err, &h) }

// Variant (b): the holder is only waiting. The lease is live by its deadline and
// dead by its activity.
func TestIdleLeaseIsTakenAndHolderTold(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("go-simd/floats", "alice", "waiting on CI", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(idleAfter + time.Minute)
	got, err := s.Acquire("go-simd/floats", "bob", "", 45*time.Minute)
	if err != nil {
		t.Fatalf("an idle lease must be takeable: %v", err)
	}
	if got.Owner != "bob" {
		t.Fatalf("owner = %q", got.Owner)
	}
	live, _, err := s.Messages("go-simd/floats", 0)
	if err != nil || len(live) != 1 || live[0].From == "alice" {
		t.Fatalf("the previous holder must be told: %v %v", live, err)
	}
	log, err := s.Takes()
	if err != nil || len(log) != 1 || log[0].From != "alice" || log[0].To != "bob" {
		t.Fatalf("the take must be logged: %v %v", log, err)
	}
}

// The comparison is >=: the threshold is stated as "idle at least N minutes".
func TestIdleThresholdBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		idle time.Duration
		take bool
	}{
		{"just under", idleAfter - time.Nanosecond, false},
		{"exactly", idleAfter, true},
		{"just over", idleAfter + time.Nanosecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(100000, 0)
			s := idleStore(t, &now)
			if _, err := s.Acquire("o/r", "alice", "", 45*time.Minute); err != nil {
				t.Fatal(err)
			}
			now = now.Add(tc.idle)
			_, err := s.Acquire("o/r", "bob", "", 45*time.Minute)
			if tc.take && err != nil || !tc.take && !isHeld(err) {
				t.Fatalf("idle %v: err = %v, want take=%v", tc.idle, err, tc.take)
			}
		})
	}
}

func TestIdleDisabledWhenZero(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	s.Idle = 0
	if _, err := s.Acquire("o/r", "alice", "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * time.Minute)
	if _, err := s.Acquire("o/r", "bob", "", time.Minute); !isHeld(err) {
		t.Fatalf("with no idle limit the deadline decides: %v", err)
	}
}

func TestTouchAndRenewKeepALeaseActive(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("o/r", "alice", "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if ok, err := s.Touch("o/r", "alice"); err != nil || !ok {
		t.Fatalf("touch: %v %v", ok, err)
	}
	if ok, _ := s.Touch("o/r", "mallory"); ok {
		t.Fatal("touch by a non-holder must not count")
	}
	now = now.Add(10 * time.Minute) // 20 min after claim, 10 after touch
	if _, err := s.Acquire("o/r", "bob", "", time.Minute); !isHeld(err) {
		t.Fatalf("touched lease taken: %v", err)
	}
	// A renewal is activity too.
	if _, err := s.Acquire("o/r", "alice", "again", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(14 * time.Minute)
	if _, err := s.Acquire("o/r", "bob", "", time.Minute); !isHeld(err) {
		t.Fatalf("renewed lease taken: %v", err)
	}
}

func TestReleaseUnlessKept(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	rel, kept, err := s.ReleaseUnlessKept("o/r", "alice")
	if err != nil || !rel || kept != "" {
		t.Fatalf("plain lease: %v %q %v", rel, kept, err)
	}
	if _, err := s.AcquireKeep("o/r", "alice", "", "release follows the merge", time.Hour); err != nil {
		t.Fatal(err)
	}
	rel, kept, err = s.ReleaseUnlessKept("o/r", "alice")
	if err != nil || rel || kept != "release follows the merge" {
		t.Fatalf("kept lease: %v %q %v", rel, kept, err)
	}
	if rel, _, _ := s.ReleaseUnlessKept("o/r", "bob"); rel {
		t.Fatal("another session's lease must never be released")
	}
	// An organisation-wide claim encloses the repo but is not this repo's lease.
	if _, err := s.Acquire("org", "carol", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if rel, _, _ := s.ReleaseUnlessKept("org/r", "carol"); rel {
		t.Fatal("a merge in org/r must not release the claim on the whole org")
	}
	if rel, _, _ := s.ReleaseUnlessKept("org", "carol"); !rel {
		t.Fatal("exact resource releases")
	}
}

// Many sessions racing for one idle lease produce one winner, and a holder
// touching at the same instant either wins (nobody takes) or loses (Touch says
// so) -- never both.
func TestConcurrentTakeAndTouch(t *testing.T) {
	for round := 0; round < 50; round++ {
		now := time.Unix(100000, 0)
		s := idleStore(t, &now)
		if _, err := s.Acquire("o/r", "alice", "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		now = now.Add(idleAfter + time.Second)
		var wg sync.WaitGroup
		var winners atomic.Int32
		start := make(chan struct{})
		var touched atomic.Bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _ := s.Touch("o/r", "alice")
			touched.Store(ok)
		}()
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := s.Acquire("o/r", "taker"+string(rune('a'+i)), "", time.Hour); err == nil {
					winners.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		w := winners.Load()
		if w > 1 {
			t.Fatalf("round %d: %d winners", round, w)
		}
		if touched.Load() == (w == 1) {
			t.Fatalf("round %d: touched=%v winners=%d, want exactly one of them", round, touched.Load(), w)
		}
		ls, _ := s.List()
		if len(ls) != 1 {
			t.Fatalf("round %d: %d leases on disk", round, len(ls))
		}
	}
}

// ---- the refusals: every error branch answers, none is silently skipped ----

func shortLocks(t *testing.T) {
	t.Helper()
	ts, tt := lockStale, lockTimeout
	lockStale, lockTimeout = time.Hour, 50*time.Millisecond
	t.Cleanup(func() { lockStale, lockTimeout = ts, tt })
}

func TestLockExcludesTimesOutAndReclaimsDebris(t *testing.T) {
	shortLocks(t)
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	p := s.path("o/r")
	unlock, err := s.lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.lock(p); err == nil || !strings.Contains(err.Error(), "lock held") {
		t.Fatalf("a held lock must time out, got %v", err)
	}
	unlock()
	unlock, err = s.lock(p)
	if err != nil {
		t.Fatalf("a released lock must be takeable: %v", err)
	}
	unlock()
	if _, err := os.Stat(p + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlock left the file: %v", err)
	}

	// Debris of a dead process: older than lockStale, removed rather than waited on.
	lockStale = time.Second
	if err := os.WriteFile(p+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	if unlock, err := s.lock(p); err != nil {
		t.Fatalf("a stale lock must be reclaimed: %v", err)
	} else {
		unlock()
	}
	// A directory that is not there is an error, not a wait.
	bad := Store{Dir: filepath.Join(t.TempDir(), "missing")}
	if _, err := bad.lock(bad.path("o/r")); err == nil || strings.Contains(err.Error(), "lock held") {
		t.Fatalf("want the open error at once, got %v", err)
	}
}

func TestEveryWriterReportsALockItCannotTake(t *testing.T) {
	shortLocks(t)
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path("o/r")+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("o/r", "bob", "", time.Hour); err == nil || isHeld(err) {
		t.Errorf("Acquire: %v", err)
	}
	if _, err := s.Touch("o/r", "alice"); err == nil {
		t.Error("Touch ignored a lock it could not take")
	}
	if _, _, err := s.ReleaseUnlessKept("o/r", "alice"); err == nil {
		t.Error("ReleaseUnlessKept ignored a lock it could not take")
	}
	if _, err := s.Release("o/r", "alice"); err == nil {
		t.Error("Release ignored a lock it could not take")
	}
}

func TestAbsentAndTornLeases(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if ok, err := s.Touch("o/none", "alice"); ok || err != nil {
		t.Errorf("Touch of nothing: %v %v", ok, err)
	}
	if rel, kept, err := s.ReleaseUnlessKept("o/none", "alice"); rel || kept != "" || err != nil {
		t.Errorf("release of nothing: %v %q %v", rel, kept, err)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path("o/torn"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Touch("o/torn", "alice"); err == nil {
		t.Error("Touch of a torn lease must say so")
	}
	if _, _, err := s.ReleaseUnlessKept("o/torn", "alice"); err == nil {
		t.Error("release of a torn lease must say so")
	}
	// A torn lease is not a holder: acquiring over it works.
	if _, err := s.Acquire("o/torn", "bob", "", time.Hour); err != nil {
		t.Errorf("a torn lease must not block: %v", err)
	}
	// An expired lease is not touched back to life.
	if _, err := s.Acquire("o/old", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if ok, _ := s.Touch("o/old", "alice"); ok {
		t.Error("Touch revived an expired lease")
	}
}

func TestAnotherSessionsLeaseIsLeftAlone(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if rel, kept, err := s.ReleaseUnlessKept("o/r", "bob"); rel || kept != "" || err != nil {
		t.Fatalf("bob releasing alice's lease: %v %q %v", rel, kept, err)
	}
	if ls, _ := s.List(); len(ls) != 1 || ls[0].Owner != "alice" {
		t.Fatalf("alice's lease was disturbed: %v", ls)
	}
}

func TestFailuresToWriteOrRemoveAreReported(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	// write goes through p+".tmp": a directory there makes it fail on every OS.
	if err := os.Mkdir(s.path("o/r")+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Touch("o/r", "alice"); ok || err == nil {
		t.Errorf("Touch with a failing write: %v %v", ok, err)
	}
	if _, err := s.Acquire("o/r", "alice", "again", time.Hour); err == nil {
		t.Error("a renewal whose write failed must say so")
	}
	if err := os.Remove(s.path("o/r") + ".tmp"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	s.remove = func(string) error { return boom }
	if rel, _, err := s.ReleaseUnlessKept("o/r", "alice"); rel || !errors.Is(err, boom) {
		t.Errorf("release with a failing remove: %v %v", rel, err)
	}
	now = now.Add(idleAfter)
	if _, err := s.Acquire("o/r", "bob", "", time.Hour); !errors.Is(err, boom) {
		t.Errorf("take with a failing remove: %v", err)
	}
	if ls, _ := s.List(); len(ls) != 1 || ls[0].Owner != "alice" {
		t.Errorf("a failed take must leave the lease as it was: %v", ls)
	}
}

func TestTakesLog(t *testing.T) {
	now := time.Unix(100000, 0)
	s := idleStore(t, &now)
	if got, err := s.Takes(); got != nil || err != nil {
		t.Fatalf("no log: %v %v", got, err)
	}
	for _, who := range []string{"bob", "carol"} {
		if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
			t.Fatal(err)
		}
		now = now.Add(idleAfter)
		if _, err := s.Acquire("o/r", who, "", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Release("o/r", who); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Takes()
	if err != nil || len(got) != 2 || got[0].To != "bob" || got[1].To != "carol" || got[0].IdleFor != "15m0s" {
		t.Fatalf("takes = %+v, %v", got, err)
	}
	// An expired lease taken over is the ordinary case and is not logged as a take.
	if _, err := s.Acquire("o/e", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := s.Acquire("o/e", "bob", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Takes(); len(got) != 2 {
		t.Errorf("an expiry was logged as a take: %+v", got)
	}
	// A log that cannot be read is an error, not "no takes".
	bad := idleStore(t, &now)
	if err := os.MkdirAll(bad.takesPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Takes(); err == nil {
		t.Error("an unreadable log read as empty")
	}
}
