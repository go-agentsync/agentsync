package lease

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func store(t *testing.T, now time.Time) Store {
	t.Helper()
	return Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
}

func TestAcquireThenRefuseAnotherOwner(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-pkgx/packages", "alice", "", time.Hour); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := s.Acquire("go-pkgx/packages", "bob", "", time.Hour)
	var held *Held
	if !errors.As(err, &held) {
		t.Fatalf("second acquire: got %v, want Held", err)
	}
	// The refusal must name the holder: "it is taken" is not actionable, and
	// knowing who lets a session ask them rather than guess.
	if held.By.Owner != "alice" {
		t.Errorf("holder = %q, want alice", held.By.Owner)
	}
}

func TestExpiredLeaseIsTakenOver(t *testing.T) {
	early := time.Unix(1000, 0)
	s := store(t, early)
	if _, err := s.Acquire("weft", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	// A session that dies holding a lease must not block the resource for the
	// days these sessions can live.
	s.Now = func() time.Time { return early.Add(2 * time.Minute) }
	got, err := s.Acquire("weft", "bob", "", time.Minute)
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if got.Owner != "bob" {
		t.Errorf("owner = %q, want bob", got.Owner)
	}
}

func TestSameOwnerExtends(t *testing.T) {
	now := time.Unix(1000, 0)
	s := store(t, now)
	first, err := s.Acquire("toolkit", "alice", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Acquire("toolkit", "alice", "still going", 2*time.Minute)
	if err != nil {
		t.Fatalf("re-acquire by the same owner must extend, not fail: %v", err)
	}
	if !second.Expires.After(first.Expires) {
		t.Errorf("expiry did not move: %v then %v", first.Expires, second.Expires)
	}
	if second.Note != "still going" {
		t.Errorf("note = %q, want the updated one", second.Note)
	}
}

func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	const n = 24 // one per session seen on the machine
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "session-" + string(rune('a'+i%26))
			if _, err := s.Acquire("contested", owner, "", time.Hour); err == nil {
				wins[i] = true
			}
		}(i)
	}
	wg.Wait()
	got := 0
	for _, w := range wins {
		if w {
			got++
		}
	}
	if got != 1 {
		t.Errorf("winners = %d, want exactly 1", got)
	}
}

func TestReleaseOnlyByOwner(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("bk", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("bk", "bob"); err == nil {
		t.Error("bob released alice's lease")
	}
	if err := s.Release("bk", "alice"); err != nil {
		t.Errorf("owner release: %v", err)
	}
	if err := s.Release("bk", "alice"); err != nil {
		t.Errorf("releasing what is already gone must be quiet: %v", err)
	}
}

func TestCorruptLeaseIsTreatedAsStale(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	// A half-written file must not wedge the resource forever.
	p := filepath.Join(s.Dir, "torn.lease")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("torn", "alice", "", time.Hour); err != nil {
		t.Fatalf("acquire over a corrupt lease: %v", err)
	}
}

func TestListReportsExpiredToo(t *testing.T) {
	early := time.Unix(1000, 0)
	s := store(t, early)
	if _, err := s.Acquire("a", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("b", "bob", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	later := early.Add(2 * time.Minute)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d, want 2", len(got))
	}
	// Hiding the stale one would hide exactly what a reader needs to see.
	expired := 0
	for _, l := range got {
		if l.Expired(later) {
			expired++
		}
	}
	if expired != 1 {
		t.Errorf("expired = %d, want 1", expired)
	}
}

func TestResourceNameSurvivesAwkwardCharacters(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	const name = "openweft/weft-app-osx"
	got, err := s.Acquire(name, "alice", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The slash must not become a directory, and the real name must come back
	// intact rather than in its flattened form.
	if got.Resource != name {
		t.Errorf("resource = %q, want %q", got.Resource, name)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Resource != name {
		t.Errorf("list = %v, %v", list, err)
	}
}
