// Package lease is an advisory lock that expires.
//
// It exists because 22 Claude sessions were found working the same project at
// once, and two of them collided in ways nothing warned about: a shared
// MEMORY.md was read-modify-written by several writers, silently dropping index
// lines, and a build campaign on go-pkgx/packages was interrupted by a manual
// dispatch from a session that had no way to know.
//
// Three properties matter, and none of them rely on a session remembering to be
// careful:
//
//  1. Acquisition is ATOMIC. The lease is a file created with O_EXCL, so two
//     sessions racing produce one winner and one clean failure, never two
//     winners.
//  2. A lease EXPIRES. A session that dies holding one -- and sessions here run
//     for days -- must not block the resource forever, so a lease past its
//     deadline is takeable by anyone. This is the difference between a lock that
//     helps and a lock that becomes the outage.
//  3. It is HONEST about being advisory. Nothing enforces it at the filesystem
//     level. It coordinates sessions that ask; it cannot stop one that does not.
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lease is what a holder writes down about itself, so a later reader can decide
// whether to wait, take over, or go elsewhere.
type Lease struct {
	Resource string    `json:"resource"`
	Owner    string    `json:"owner"`
	PID      int       `json:"pid"`
	Note     string    `json:"note,omitempty"`
	Acquired time.Time `json:"acquired"`
	Expires  time.Time `json:"expires"`
	// Active is the last moment the holder DID something with the lease: claimed,
	// renewed it, or published into the resource. Absent in a lease written by an
	// older agentsync, which reads as Acquired.
	Active time.Time `json:"active,omitempty"`
	// Keep is the reason the holder wants the lease to survive a successful
	// publish. Without it a ghmerge/ghrelease that succeeds releases the lease.
	Keep string `json:"keep,omitempty"`
}

// LastActive is when the holder last acted on the lease.
func (l Lease) LastActive() time.Time {
	if l.Active.IsZero() {
		return l.Acquired
	}
	return l.Active
}

// Idle reports whether the holder has done nothing with the lease for at least
// threshold. A zero or negative threshold disables the test.
func (l Lease) Idle(now time.Time, threshold time.Duration) bool {
	return threshold > 0 && now.Sub(l.LastActive()) >= threshold
}

// Takeable reports whether another session may take the lease: its deadline has
// passed, or its holder has been idle for at least idle.
func (l Lease) Takeable(now time.Time, idle time.Duration) bool {
	return l.Expired(now) || l.Idle(now, idle)
}

// Expired reports whether the lease may be taken from its holder.
func (l Lease) Expired(now time.Time) bool { return now.After(l.Expires) }

// Held is returned when the resource belongs to someone else and has not
// expired. It carries the holder so a caller can say who, rather than only that
// it failed.
type Held struct{ By Lease }

func (e *Held) Error() string {
	return fmt.Sprintf("held by %s until %s", e.By.Owner, e.By.Expires.Format(time.RFC3339))
}

// Store is a directory of leases.
type Store struct {
	Dir string
	Now func() time.Time // injectable so the tests do not sleep
	// Idle is how long a holder may do nothing before another session may take
	// the lease. Zero disables it: a lease then lasts to its deadline.
	Idle time.Duration

	remove func(string) error         // nil is os.Remove; a test's way to make removal fail
	rename func(string, string) error // nil is os.Rename; the same, for a queue's write
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// forgeHosts are the hosts a resource may be written with in front of it.
//
// The list is closed on purpose. Guessing at "anything that looks like a host"
// would have to decide what ".github" is -- a real repository name in every
// organisation of this fleet, and one that a dot-means-host rule mistakes for a
// domain. A name this does not recognise is left exactly as written.
var forgeHosts = map[string]bool{
	"github.com": true, "www.github.com": true,
	"gitlab.com": true, "codeberg.org": true, "bitbucket.org": true,
}

// Normalize reduces a resource to the one key two sessions naming the same
// thing differently have to land on.
//
// It exists because they did not. One session claimed "go-ansible/.github" and
// another "github.com/go-ansible/.github"; both were granted, both believed
// they held the repository, and the tool written to stop exactly that collision
// reported no conflict because the two strings differ.
//
// A URL, an SSH remote and a bare owner/repo all come to the same key, and case
// is not part of the identity: GitHub itself treats owner and repository names
// case-insensitively.
//
// ⛔ IT RUNS TO A FIXED POINT. The gate normalizes a repository once (ownerRepo)
// and the store normalizes it again (Covering, path), so a key that one pass
// moved and a second pass moved again named two different resources depending
// on how many callers it went through. FuzzNormalizePath found it on its first
// run: "githuB.Com/ 0" -> " 0" -> "0". Every step only shortens its input or
// leaves it (lower-casing is idempotent), so the loop ends.
//
// ⛔ AND THE BOUND IS WRITTEN DOWN, because that last sentence is an argument
// about normalizeOnce and not a property of this loop. `mutsweep` negated the
// comparison below and the mutation HUNG rather than failing a test: the only
// thing standing between this function and an endless loop was one `==`. Every
// gate runs on every Bash call and normalizes names that come from other
// sessions' lease files and from the hook payload, so an edit to normalizeOnce
// that ever grows a string, or cycles between two, would hang the gate on every
// command this machine runs instead of being caught by a test.
//
// A continuing pass strictly shortens s, so len(s)+1 passes cannot be exceeded
// by any input that converges. Reaching the bound returns the last value: a key
// that is merely imperfect, rather than a command that never returns.
func Normalize(resource string) string {
	s := normalizeStep(resource)
	// ⛔ The limit is taken ONCE. Written as `i <= len(s)` it is re-measured every
	// pass, so a step that grows its input outruns its own bound: measured, the
	// loop was still running after 226,430 passes.
	limit := len(s) + 1
	for i := 0; i < limit; i++ {
		next := normalizeStep(s)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

// normalizeStep is normalizeOnce; a test replaces it, because a step that does
// not converge is the one case this bound exists for and no input can provoke it
// through the real one (the same reason statFile is a var in cmd/agentsync).
var normalizeStep = normalizeOnce

func normalizeOnce(resource string) string {
	s := strings.TrimSpace(resource)
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, scheme)
	}
	// An SSH remote carries a user in front of the host, in either of the two
	// shapes git uses: "user@host:owner/repo" and "user@host/owner/repo". The
	// user is dropped in both; the host that may follow is left for the forge
	// list below to decide about. Only what precedes the "@" is examined, and
	// only when it is not itself a path, so a resource that merely contains an
	// "@" is left alone.
	if i := strings.Index(s, "@"); i >= 0 && !strings.Contains(s[:i], "/") {
		rest := s[i+1:]
		if j := strings.Index(rest, ":"); j >= 0 && !strings.Contains(rest[:j], "/") {
			rest = rest[j+1:]
		}
		s = rest
	}
	s = strings.ToLower(strings.Trim(s, "/"))
	parts := strings.Split(s, "/")
	if len(parts) > 1 && forgeHosts[parts[0]] {
		parts = parts[1:]
	}
	if n := len(parts); n > 0 {
		parts[n-1] = strings.TrimSuffix(parts[n-1], ".git")
	}
	return strings.Join(parts, "/")
}

// covers reports whether a names the same resource as b or one enclosing it.
//
// The comparison is by path SEGMENT and not by string prefix: "go-tex" must not
// be read as enclosing "go-texinfo", which a prefix test would say it does.
func covers(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	if len(as) > len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// bareNames reports whether bare is a single-segment name that is the
// repository part of qualified: "optimize" and "go-numerics/optimize".
//
// The shared directory holds claims written both ways, so two sessions could
// each hold "the same repository" with neither acquisition seeing the other --
// and a publish gate that took the bare one for the whole answer would let one
// of them in.
func bareNames(bare, qualified string) bool {
	if strings.Contains(bare, "/") {
		return false
	}
	segs := strings.Split(qualified, "/")
	return len(segs) > 1 && segs[len(segs)-1] == bare
}

// dashNames reports whether dashed is the CLONE DIRECTORY spelling of
// qualified: "go-gitsafe-gitsafe" for "go-gitsafe/gitsafe".
//
// ⛔ MEASURED LIVE, 2026-11-22 ~23:00: `go-gitsafe/gitsafe` held by one session
// and `go-gitsafe-gitsafe` by another, at the same time, on this machine. A
// subagent then waited for the first while the second was the lease that
// mattered, so it queued behind the wrong key. bareNames does not see this pair
// -- the dash form has no slash, and its last segment is the whole string --
// which is why the collision survived the fix that bareNames was written for.
//
// The convention it reads is `~/Documents/VCS/GIT/localhost/<org>-<repo>` (this
// machine's layout), which is the name a session has IN FRONT OF IT while
// working, and therefore the name it reaches for.
//
// ⛔ The comparison is an EXACT equality against a key that already exists, not
// a split of the dash form. "go-tex-engine" could be read as go-tex/engine or
// go/tex-engine, and nothing in the string says which; splitting it would
// invent an owner. Asking instead whether some LIVE lease spells itself o/r
// such that o+"-"+r is this name cannot invent anything, and when two live
// leases both match, refusing against either is still correct -- the point is
// that the session is told to go and look.
//
// A deeper path is not a clone directory: "a/b/c" has no `<org>-<repo>` form,
// so it is left alone. Nor is a worktree TOPIC: `<org>-<repo>-<topic>` is how
// this machine names a second checkout of one repository, and sessions claim
// those separately on purpose, so only the exact two-part name collides.
//
// ⛔ There is deliberately NO `strings.Contains(dashed, "/")` guard, although
// bareNames has one: by the time the comparison runs, owner holds what precedes
// the first slash and repo has been refused if it holds another, so owner+"-"+
// repo contains no slash at all and a dashed form that holds one cannot equal
// it. A mutation sweep said so before this comment did -- the guard SURVIVED,
// because nothing it did could change an answer.
func dashNames(dashed, qualified string) bool {
	owner, repo, ok := strings.Cut(qualified, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return false
	}
	return dashed == owner+"-"+repo
}

// SameRepository reports whether two NORMALISED keys name one repository under
// the spellings this machine produces: the qualified `owner/repo`, the bare
// `repo`, and the clone directory `owner-repo`. It is symmetric, and it is
// deliberately NOT `covers`: an organisation encloses a repository without
// being it, and a caller asking "is this lease about exactly this repository"
// must not be handed the lease on everything around it.
//
// ⛔ It exists because the publish gate had its own copy of the rule, matching
// the qualified and bare forms only, and so never released a lease taken under
// the clone directory name: the spellings were made equal for CLAIMING in one
// place and left unequal for RELEASING in another. One repository's spellings
// are decided here, once, and every caller asks this.
func SameRepository(a, b string) bool {
	return a == b ||
		bareNames(a, b) || bareNames(b, a) ||
		dashNames(a, b) || dashNames(b, a)
}

// overlapping reports a live lease that encloses this resource or sits inside
// it, held by somebody else.
//
// An organisation and one of its repositories are not the same key and never
// will be, so nothing but this makes them collide -- and two sessions working
// the same repository, one having claimed the organisation around it, is the
// collision this tool exists for.
//
// What is skipped is the lease living at the FILE this claim is about to take,
// and nothing else: that one is left to the O_EXCL path below, which is what
// makes taking over an expired lease and extending one's own work.
//
// Skipping by KEY instead was wrong, and wrong exactly where it mattered. A
// lease written before this normalisation existed sits under the old file name
// while its recorded resource still normalises to the new key, so a key test
// skipped it and the O_EXCL path never saw it either -- the claim went through
// against a live holder. Comparing the file is what makes an old lease and a
// new one meet.
func (s Store) overlapping(target, key, owner string, now time.Time) *Lease {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil // a directory that cannot be listed is not a conflict
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lease") {
			continue
		}
		p := filepath.Join(s.Dir, e.Name())
		if p == target {
			continue
		}
		l, err := s.read(p)
		if err != nil {
			continue // a torn file is not a holder
		}
		if l.Owner == owner || l.Takeable(now, s.Idle) {
			continue
		}
		if overlaps(Normalize(l.Resource), key) {
			held := l
			return &held
		}
	}
	return nil
}

// overlaps reports whether two normalised keys are one claim: one encloses the
// other, or they are the bare, dashed and slashed names of one repository. The
// lease conflict and the waiting line use this one test, so a spelling that
// would be refused as Held cannot jump the line either.
func overlaps(a, b string) bool {
	return covers(a, b) || covers(b, a) || bareNames(a, b) || bareNames(b, a) ||
		dashNames(a, b) || dashNames(b, a)
}

// path maps a resource to a file name. The resource is normalised first, so two
// spellings of one repository are one file. Slashes and anything else awkward
// become underscores so "owner/repo" is flat, and the original name is kept
// inside the file rather than encoded into it.
func (s Store) path(resource string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, Normalize(resource))
	if len(safe) > 120 {
		safe = safe[:120]
	}
	return filepath.Join(s.Dir, safe+".lease")
}

// Acquire takes the resource for ttl, or reports who holds it.
//
// A lease whose deadline has passed is taken over rather than respected: the
// alternative is that one dead session blocks a resource until a human notices.
//
// A lease whose holder has been idle for Store.Idle is taken over the same way:
// the deadline says how long the holder MAY hold it, the activity says whether
// anyone is using it, and a holder that is only waiting (on CI, on load, on a
// quota) is not.
func (s Store) Acquire(resource, owner, note string, ttl time.Duration) (Lease, error) {
	return s.AcquireKeep(resource, owner, note, "", ttl)
}

// AcquireKeep is Acquire with a reason the lease should survive a successful
// publish (see ReleaseUnlessKept).
func (s Store) AcquireKeep(resource, owner, note, keep string, ttl time.Duration) (Lease, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Lease{}, err
	}
	now := s.now()
	l := Lease{
		Resource: resource, Owner: owner, PID: os.Getpid(), Note: note,
		Acquired: now, Expires: now.Add(ttl), Active: now, Keep: keep,
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lease{}, err
	}
	p := s.path(resource)
	if held := s.overlapping(p, Normalize(resource), owner, now); held != nil {
		return Lease{}, &Held{By: *held}
	}
	// ⛔ FIRST COME, FIRST SERVED (queue.go says why). Sessions waiting longer
	// get a lease that is free or takeable before this one does. A live lease
	// held by someone else is still reported as Held -- who holds it is the more
	// useful answer -- and a holder extending its own lease jumps nobody.
	line, err := s.waitersAhead(resource, owner, now)
	if err != nil {
		return Lease{}, err
	}
	if len(line) > 0 {
		cur, rerr := s.read(p)
		switch {
		case rerr == nil && !cur.Takeable(now, s.Idle) && cur.Owner == owner:
			// Extending a LIVE lease of one's own. An expired or idle one is free
			// by this package's rule, and its former holder queues like anyone.
		case rerr == nil && !cur.Takeable(now, s.Idle):
			return Lease{}, &Held{By: cur}
		default:
			return Lease{}, &Queued{Resource: resource, Ahead: line}
		}
	}
	got, err := s.acquire(p, body, l, owner, now, resource)
	if err == nil {
		// Best effort: the lease is ours, and a place left in line lapses on its
		// own at its Until.
		_ = s.Leave(resource, owner)
	}
	return got, err
}

func (s Store) acquire(p string, body []byte, l Lease, owner string, now time.Time, resource string) (Lease, error) {
	for attempt := 0; attempt < 2; attempt++ {
		err := createExclusive(p, body)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return Lease{}, err
		}
		// Someone holds it. Take over only if their deadline has passed or they
		// have gone idle -- decided UNDER THE FILE'S LOCK, because the decision
		// (read, judge, remove, create) is four steps and a holder renewing between
		// the read and the remove would otherwise lose a lease it had just renewed.
		got, done, err := s.takeOver(p, body, l, owner, now)
		if err != nil {
			return Lease{}, err
		}
		if done {
			return got, nil
		}
	}
	return Lease{}, fmt.Errorf("lease: %s: lost the race twice", resource)
}

// createExclusive puts a COMPLETE lease at p, or fails because one is already
// there. Nothing in between is ever visible.
//
// Creating the file with O_EXCL and writing it afterwards is exclusive about
// the NAME and not about the CONTENT: between the create and the write the
// file exists and is empty. A second session arriving in that window reads an
// empty file, cannot parse it, and -- following the rule that an unreadable
// lease is a torn one -- DELETES it and takes the resource. Both sessions then
// believe they hold it.
//
// That is not a hypothesis. The package's own concurrency test finds it: 24
// racing sessions produce two or three winners about once in a hundred runs
// (`go test -count=300 -run Concurrent`), and it had been there since the
// package was written. It matters more than the odds suggest, because the two
// winners never learn of each other -- which is the exact failure this package
// exists to prevent.
//
// Writing the body first and then linking it into place fixes it: link is
// atomic and refuses an existing name, so the lease is whole from the instant
// it is visible.
func createExclusive(p string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".lease-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := tmpWrite(f, body); err != nil {
		f.Close()
		return err
	}
	if err := tmpClose(f); err != nil {
		return err
	}
	return os.Link(tmp, p)
}

// The three file operations a real filesystem cannot be made to fail portably
// (a short write, a failing close, a rename that loses to a permission change
// after the temporary was written). They are variables so a test can make them
// fail; in production they are the plain os calls.
var (
	tmpWrite   = func(f *os.File, b []byte) error { _, err := f.Write(b); return err }
	tmpClose   = func(f *os.File) error { return f.Close() }
	renameFile = os.Rename
)

func (s Store) write(p string, l Lease) (Lease, error) {
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lease{}, err
	}
	// Write-then-rename so a reader never sees a half-written lease.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return Lease{}, err
	}
	if err := renameFile(tmp, p); err != nil {
		return Lease{}, err
	}
	return l, nil
}

func (s Store) read(p string) (Lease, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Lease{}, err
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return Lease{}, err
	}
	if l.Resource == "" || l.Owner == "" {
		return Lease{}, errors.New("lease: incomplete record")
	}
	return l, nil
}

// Release drops the lease if we hold it, and reports whether there was one to
// drop. A lease held by another session is an error worth seeing: it means two
// sessions disagree about who owns what. A lease that is simply gone is not an
// error -- releasing twice is allowed -- but the caller is told, because
// "released" said of something never held carries no information, and a success
// message that cannot fail is indistinguishable from one that did nothing.
func (s Store) Release(resource, owner string) (bool, error) {
	p := s.path(resource)
	unlock, err := s.lock(p)
	if err != nil {
		return false, err
	}
	defer unlock()
	cur, err := s.read(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cur.Owner != owner {
		return false, &Held{By: cur}
	}
	if err := s.rm(p); err != nil {
		return false, err
	}
	return true, nil
}

// Covering returns every lease, expired ones included, that is about the same
// resource as the one asked about or encloses it: "go-numerics/optimize" is
// covered by a lease on itself and by one on "go-numerics".
//
// A lease written with the bare repository name ("optimize") also covers an
// owner/repo ending in it: the shared directory holds such leases, and a gate
// that missed them would refuse the one session that did claim.
func (s Store) Covering(resource string) ([]Lease, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	key := Normalize(resource)
	var out []Lease
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lease") {
			continue
		}
		l, err := s.read(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			// Unlike List, which skips a torn file to show the rest, this cannot:
			// the unreadable one may be the holder, and answering "nobody holds
			// it" over it is a claim about a file nobody read.
			return nil, fmt.Errorf("lease %s is unreadable: %w", e.Name(), err)
		}
		k := Normalize(l.Resource)
		// dashNames in this direction only: a lease written as the clone
		// directory ("go-gitsafe-gitsafe") covers a publish into the repository
		// it is a checkout of. The other direction would be a lease on the
		// repository covering a question about a directory, which no gate asks.
		if covers(k, key) || bareNames(k, key) || dashNames(k, key) {
			out = append(out, l)
		}
	}
	return out, nil
}

// List returns every lease, expired ones included, so a caller can show what is
// stale rather than hide it.
func (s Store) List() ([]Lease, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Lease
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lease") {
			continue
		}
		l, err := s.read(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue // a torn file is not worth failing the whole listing
		}
		out = append(out, l)
	}
	return out, nil
}
