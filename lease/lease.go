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
func Normalize(resource string) string {
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
		k := Normalize(l.Resource)
		if l.Owner == owner || l.Expired(now) {
			continue
		}
		if covers(k, key) || covers(key, k) {
			held := l
			return &held
		}
	}
	return nil
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
func (s Store) Acquire(resource, owner, note string, ttl time.Duration) (Lease, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Lease{}, err
	}
	now := s.now()
	l := Lease{
		Resource: resource, Owner: owner, PID: os.Getpid(), Note: note,
		Acquired: now, Expires: now.Add(ttl),
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lease{}, err
	}
	p := s.path(resource)
	if held := s.overlapping(p, Normalize(resource), owner, now); held != nil {
		return Lease{}, &Held{By: *held}
	}

	for attempt := 0; attempt < 2; attempt++ {
		err := createExclusive(p, body)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return Lease{}, err
		}
		// Someone holds it. Take over only if their deadline has passed.
		cur, rerr := s.read(p)
		if rerr != nil {
			// Unreadable or half-written: treat as stale rather than wedge
			// forever on a corrupt file.
			if rmErr := os.Remove(p); rmErr != nil {
				return Lease{}, rmErr
			}
			continue
		}
		if cur.Owner == owner {
			// Re-entrant: extend our own rather than fail.
			return s.write(p, l)
		}
		if !cur.Expired(now) {
			return Lease{}, &Held{By: cur}
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Lease{}, err
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
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Link(tmp, p)
}

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
	if err := os.Rename(tmp, p); err != nil {
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
	if err := os.Remove(p); err != nil {
		return false, err
	}
	return true, nil
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
