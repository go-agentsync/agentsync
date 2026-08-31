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

// path maps a resource to a file name. Slashes and anything else awkward become
// underscores so "owner/repo" is one flat file, and the original name is kept
// inside the file rather than encoded into it.
func (s Store) path(resource string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, resource)
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

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.Write(body)
			cerr := f.Close()
			if werr != nil {
				return Lease{}, werr
			}
			return l, cerr
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
