package lease

import (
	"errors"
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
