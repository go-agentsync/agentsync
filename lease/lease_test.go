package lease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := s.Release("bk", "bob"); err == nil {
		t.Error("bob released alice's lease")
	}
	dropped, err := s.Release("bk", "alice")
	if err != nil {
		t.Errorf("owner release: %v", err)
	}
	if !dropped {
		t.Error("owner release reported nothing to drop")
	}
	dropped, err = s.Release("bk", "alice")
	if err != nil {
		t.Errorf("releasing what is already gone must be quiet: %v", err)
	}
	if dropped {
		t.Error("releasing what is already gone claimed to drop a lease")
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

func TestTwoSpellingsOfOneRepositoryAreOneClaim(t *testing.T) {
	// This is the collision the normalisation exists for. One session claimed
	// "go-ansible/.github" and another "github.com/go-ansible/.github"; both
	// were granted and both believed they held the repository.
	for _, spelling := range []string{
		"github.com/go-ansible/.github",
		"https://github.com/go-ansible/.github",
		"https://github.com/go-ansible/.github.git",
		"git@github.com:go-ansible/.github",
		"GitHub.com/Go-Ansible/.GitHub",
		"go-ansible/.github/",
	} {
		t.Run(spelling, func(t *testing.T) {
			s := store(t, time.Unix(1000, 0))
			if _, err := s.Acquire("go-ansible/.github", "alice", "", time.Hour); err != nil {
				t.Fatalf("first acquire: %v", err)
			}
			_, err := s.Acquire(spelling, "bob", "", time.Hour)
			var held *Held
			if !errors.As(err, &held) {
				t.Fatalf("acquiring %q after the bare name: got %v, want Held", spelling, err)
			}
			if held.By.Owner != "alice" {
				t.Errorf("holder = %q, want alice", held.By.Owner)
			}
		})
	}
}

func TestWhatNormalisingDoesAndDoesNotTouch(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"go-pdfkit/ops", "go-pdfkit/ops"},
		{"github.com/go-pdfkit/ops", "go-pdfkit/ops"},
		{"https://github.com/go-pdfkit/ops.git", "go-pdfkit/ops"},
		{"git@github.com:go-pdfkit/ops.git", "go-pdfkit/ops"},
		{"ssh://git@github.com/go-pdfkit/ops", "go-pdfkit/ops"},
		{"codeberg.org/someone/thing", "someone/thing"},
		// ".github" is a real repository in every organisation of this fleet.
		// A rule that reads "a dot means a host" would eat it.
		{"go-ansible/.github", "go-ansible/.github"},
		{"github.com/go-ansible/.github", "go-ansible/.github"},
		// A host this does not know is left exactly as written rather than
		// guessed at.
		{"example.invalid/o/r", "example.invalid/o/r"},
		// Not a repository at all, and none of this should disturb it.
		{"localhost/tools/agentsync", "localhost/tools/agentsync"},
		{"  spaced/out  ", "spaced/out"},
	} {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnOrganisationEnclosesItsRepositories(t *testing.T) {
	// Two sessions working the same repository, one having claimed the
	// organisation around it, is the collision this tool exists for -- and
	// nothing but the segment comparison makes those two keys meet.
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-ansible", "alice", "docs pass", time.Hour); err != nil {
		t.Fatalf("claiming the organisation: %v", err)
	}
	_, err := s.Acquire("github.com/go-ansible/.github", "bob", "", time.Hour)
	var held *Held
	if !errors.As(err, &held) {
		t.Fatalf("claiming a repository inside it: got %v, want Held", err)
	}
	if held.By.Note != "docs pass" {
		t.Errorf("the refusal carries note %q, want the holder's own", held.By.Note)
	}
	// And the other way round: the repository taken first blocks the whole
	// organisation, because a sweep over it would reach the held one.
	s2 := store(t, time.Unix(1000, 0))
	if _, err := s2.Acquire("go-ansible/docs", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Acquire("go-ansible", "bob", "", time.Hour); !errors.As(err, &held) {
		t.Errorf("claiming the organisation around a held repository: got %v, want Held", err)
	}
}

func TestOneNameIsNotInsideAnotherJustBecauseItStartsTheSame(t *testing.T) {
	// A string-prefix test would read "go-tex" as enclosing "go-texinfo" and
	// refuse a claim that overlaps nothing at all.
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-tex", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-texinfo", "bob", "", time.Hour); err != nil {
		t.Errorf("go-texinfo was refused for looking like go-tex: %v", err)
	}
	if _, err := s.Acquire("go-tex-engine/parser", "bob", "", time.Hour); err != nil {
		t.Errorf("go-tex-engine/parser was refused: %v", err)
	}
}

func TestAnExpiredEnclosingClaimDoesNotBlock(t *testing.T) {
	early, late := time.Unix(1000, 0), time.Unix(1000, 0).Add(2*time.Hour)
	dir := t.TempDir()
	old := Store{Dir: dir, Now: func() time.Time { return early }}
	if _, err := old.Acquire("go-ansible", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	now := Store{Dir: dir, Now: func() time.Time { return late }}
	if _, err := now.Acquire("go-ansible/docs", "bob", "", time.Hour); err != nil {
		t.Errorf("a dead session's organisation claim blocked a repository: %v", err)
	}
}

func TestASessionIsNotBlockedByItsOwnEnclosingClaim(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-ansible", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-ansible/docs", "alice", "", time.Hour); err != nil {
		t.Errorf("alice was refused her own organisation's repository: %v", err)
	}
}

func TestReleasingWhatWasClaimedUnderAnotherSpelling(t *testing.T) {
	s := store(t, time.Unix(1000, 0))
	if _, err := s.Acquire("https://github.com/go-pdfkit/ops.git", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Release("go-pdfkit/ops", "alice")
	if err != nil || !ok {
		t.Errorf("Release(bare name) = %v, %v; want true, nil", ok, err)
	}
}

func TestAnUnlistableDirectoryIsNotReadAsAConflict(t *testing.T) {
	// A store whose directory does not exist yet has no leases in it, and a
	// claim against it must go through rather than be refused by a listing
	// that failed.
	s := Store{Dir: filepath.Join(t.TempDir(), "not", "there"),
		Now: func() time.Time { return time.Unix(1000, 0) }}
	if _, err := s.Acquire("go-pdfkit/ops", "alice", "", time.Hour); err != nil {
		t.Errorf("first claim in a fresh directory: %v", err)
	}
}

func TestADirectoryThatCannotBeListedIsNotReadAsAConflict(t *testing.T) {
	// A store whose directory is not a directory cannot be listed, and the
	// overlap check has to decide what that means. It means "no conflict
	// established", not "conflict": refusing every claim because a listing
	// failed would turn an unreadable directory into a fleet-wide outage,
	// which is the failure mode this whole package was written against.
	dir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(dir, []byte("this is a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
	if _, err := s.Acquire("go-pdfkit/ops", "alice", "", time.Hour); err == nil {
		t.Error("acquiring in an unusable directory succeeded; it should fail on the WRITE")
	} else if _, held := err.(*Held); held {
		t.Errorf("an unreadable directory was reported as a holder: %v", err)
	}
	// Acquire never reaches the overlap check here -- it fails earlier, on
	// making the directory -- so the decision itself is asked directly.
	if got := s.overlapping(s.path("go-pdfkit/ops"), "go-pdfkit/ops", "alice", time.Unix(1000, 0)); got != nil {
		t.Errorf("an unlistable directory reported %+v as a holder", *got)
	}
}

func TestALeaseIsNeverVisibleHalfWritten(t *testing.T) {
	// The defect this guards: creating the file with O_EXCL and writing it
	// afterwards is exclusive about the NAME and not the CONTENT. A session
	// arriving between the two reads an empty file, cannot parse it, and --
	// following the rule that an unreadable lease is a torn one -- deletes it
	// and takes the resource. Both then believe they hold it.
	//
	// Measured before the fix: 24 racing sessions produced two or three
	// winners about once in a hundred runs.
	dir := t.TempDir()
	p := filepath.Join(dir, "contested.lease")
	body := []byte(`{"resource":"contested","owner":"alice","acquired":"2026-09-06T00:00:00Z","expires":"2026-09-06T01:00:00Z"}`)

	const n = 24
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wins[i] = createExclusive(p, body) == nil
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
	// And what is on disk is a whole lease, not a fragment.
	s := Store{Dir: dir}
	l, err := s.read(p)
	if err != nil {
		t.Fatalf("the lease that was written does not read back: %v", err)
	}
	if l.Owner != "alice" {
		t.Errorf("owner = %q", l.Owner)
	}
	// No temporary file is left behind to be mistaken for one.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file survived: %s", e.Name())
		}
	}
}

func TestCreatingALeaseWhereThereIsNoDirectory(t *testing.T) {
	// Acquire makes the directory first, so this path is asked directly.
	err := createExclusive(filepath.Join(t.TempDir(), "no", "such", "x.lease"), []byte("{}"))
	if err == nil {
		t.Error("creating a lease under a directory that does not exist succeeded")
	}
	if errors.Is(err, os.ErrExist) {
		t.Errorf("a missing directory was reported as an existing lease: %v", err)
	}
}

func TestALeaseWrittenUnderTheOldFileNameStillHolds(t *testing.T) {
	// Leases written before the normalisation existed sit under a file named
	// from what was typed. Their recorded resource still normalises to the new
	// key, so a check that skipped "the same key, left to O_EXCL" skipped them
	// -- and O_EXCL never saw them either, because the file name differs. The
	// claim then went through against a live holder, which is precisely the
	// collision all of this is for.
	dir := t.TempDir()
	legacy := filepath.Join(dir, "github.com_go-ansible_.github.lease")
	body := []byte(`{"resource":"github.com/go-ansible/.github","owner":"peer",` +
		`"note":"docs pass","acquired":"2026-09-06T00:00:00Z","expires":"2126-09-06T00:00:00Z"}`)
	if err := os.WriteFile(legacy, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
	_, err := s.Acquire("go-ansible/.github", "me", "", time.Hour)
	var held *Held
	if !errors.As(err, &held) {
		t.Fatalf("claiming what a legacy lease holds: got %v, want Held", err)
	}
	if held.By.Owner != "peer" || held.By.Note != "docs pass" {
		t.Errorf("the refusal names %q (%q)", held.By.Owner, held.By.Note)
	}
}

func TestAnExpiredLeaseUnderTheOldFileNameDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"resource":"github.com/go-ansible/.github","owner":"peer",` +
		`"acquired":"2020-01-01T00:00:00Z","expires":"2020-01-01T01:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(dir, "github.com_go-ansible_.github.lease"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir, Now: func() time.Time { return time.Unix(1<<31, 0) }}
	if _, err := s.Acquire("go-ansible/.github", "me", "", time.Hour); err != nil {
		t.Errorf("a dead session's old-format lease blocked the claim: %v", err)
	}
}
