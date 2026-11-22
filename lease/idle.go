package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockStale is how old a per-lease lock file may get before it is taken to be
// the debris of a process that died inside the critical section, which is a few
// file operations long.
const lockStale = 10 * time.Second

// lock serialises every read-judge-write on ONE lease file. It is a file made
// with O_EXCL, so it needs nothing from the platform; a lock older than
// lockStale is removed rather than waited on, because a lock that can wedge is
// the outage this package exists to prevent.
func (s Store) lock(p string) (unlock func(), err error) {
	lp := p + ".lock"
	deadline := time.Now().Add(2 * lockStale)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lp) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(lp); serr == nil && time.Since(info.ModTime()) > lockStale {
			os.Remove(lp)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lease: %s: lock held for over %v", filepath.Base(p), 2*lockStale)
		}
		time.Sleep(time.Millisecond)
	}
}

// takeOver is the part of Acquire that runs when the lease file exists: extend
// our own, refuse a live other, or replace a takeable one. done is false when
// the file vanished or was torn and the caller should try the create again.
func (s Store) takeOver(p string, l Lease, owner string, now time.Time) (got Lease, done bool, err error) {
	unlock, err := s.lock(p)
	if err != nil {
		return Lease{}, false, err
	}
	defer unlock()
	cur, rerr := s.read(p)
	if errors.Is(rerr, os.ErrNotExist) {
		return Lease{}, false, nil
	}
	if rerr != nil {
		// Unreadable or half-written: treat as stale rather than wedge forever
		// on a corrupt file.
		if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return Lease{}, false, rmErr
		}
		return Lease{}, false, nil
	}
	if cur.Owner == owner {
		// Re-entrant: extend our own rather than fail.
		w, err := s.write(p, l)
		return w, err == nil, err
	}
	if !cur.Takeable(now, s.Idle) {
		return Lease{}, false, &Held{By: cur}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Lease{}, false, err
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lease{}, false, err
	}
	if err := createExclusive(p, body); err != nil {
		return Lease{}, false, err
	}
	if !cur.Expired(now) {
		// Taken while its deadline had NOT passed: only its inactivity justified
		// it, so say so -- to the holder, who finds out on its next agentsync call
		// instead of at a refused push, and in a log anyone can read.
		s.recordTake(cur, owner, now)
	}
	return l, true, nil
}

// Take is one recorded take-over of an idle lease.
type Take struct {
	Resource string    `json:"resource"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	At       time.Time `json:"at"`
	IdleFor  string    `json:"idle_for"`
}

func (s Store) takesPath() string { return filepath.Join(s.Dir, "takes.log") }

// recordTake is best effort by design: the lease has already changed hands, and
// failing the claim because a note could not be written would put it back.
func (s Store) recordTake(prev Lease, to string, now time.Time) {
	idle := now.Sub(prev.LastActive()).Round(time.Second)
	t := Take{Resource: prev.Resource, From: prev.Owner, To: to, At: now, IdleFor: idle.String()}
	if body, err := json.Marshal(t); err == nil {
		if f, err := os.OpenFile(s.takesPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(body, '\n'))
			f.Close()
		}
	}
	_, _ = s.Tell(prev.Resource, "agentsync", fmt.Sprintf(
		"your lease on %s was taken by %s at %s after %s with no activity from you (idle limit %s). "+
			"Claim it again before you publish: agentsync claim %s",
		prev.Resource, to, now.Format("15:04:05"), idle, s.Idle, prev.Resource))
}

// Takes reads the take-over log, oldest first. A missing log is no takes.
func (s Store) Takes() ([]Take, error) {
	b, err := os.ReadFile(s.takesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Take
	for _, line := range splitLines(b) {
		var t Take
		if json.Unmarshal(line, &t) == nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// Touch records that owner just acted on resource, so an active holder is never
// mistaken for an idle one. It reports whether owner holds the lease; a lease
// that is someone else's, gone, or already past its deadline is not touched.
func (s Store) Touch(resource, owner string) (bool, error) {
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
	if cur.Owner != owner || cur.Expired(s.now()) {
		return false, nil
	}
	cur.Active = s.now()
	if _, err := s.write(p, cur); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseUnlessKept drops owner's own lease on exactly this resource, unless
// it carries a Keep reason, in which case it is left and the reason returned.
// It is what a successful publish calls: the work the lease was for is done.
//
// "Exactly": a claim on the organisation around a repository is not that
// repository's lease, and one merge says nothing about the rest of it.
func (s Store) ReleaseUnlessKept(resource, owner string) (released bool, kept string, err error) {
	p := s.path(resource)
	unlock, err := s.lock(p)
	if err != nil {
		return false, "", err
	}
	defer unlock()
	cur, err := s.read(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if cur.Owner != owner {
		return false, "", nil
	}
	if cur.Keep != "" {
		return false, cur.Keep, nil
	}
	if err := os.Remove(p); err != nil {
		return false, "", err
	}
	return true, "", nil
}
