package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The queue exists because waiting was a lottery.
//
// `claim --wait` polled: every waiting session asked again every 15 s, and the
// first Acquire to land after the lease lapsed won it, whoever had been waiting
// longest. On 2026-11-27/28 one session waited for go-fleettools/fleettools for
// over an hour across nine 9-minute waits, a CI-green pull request ready to
// merge, and lost every hand-over -- to 229f8a94, d42196df (three times),
// d09fa1d3, 6899659a, 1dfa8e45, d7c789b4 and fd117ff7 -- while three more
// sessions queued behind it in messages that nothing read in order.
//
// The fix is in the STORE, not in the waiting loop, so that it holds for every
// way of asking: a plain `claim` that arrives while others wait is refused with
// the line it would have jumped, exactly as a waiting one is.

// Waiter is one session waiting its turn for a resource.
type Waiter struct {
	// Resource is the name the waiter asked for, so a line can be matched
	// against every spelling that would conflict with it (see overlaps).
	Resource string    `json:"resource,omitempty"`
	Owner    string    `json:"owner"`
	Since    time.Time `json:"since"`
	// Until is when the place lapses unless the waiter refreshes it. A waiter
	// that died, gave up or whose session ended must not hold the line for
	// ever: that would be the outage this package exists to prevent, moved
	// from the lease to the queue.
	Until time.Time `json:"until"`
}

// Queued is returned when the resource could be had but other sessions have
// been waiting for it longer. Ahead is them, longest-waiting first.
type Queued struct {
	Resource string
	Ahead    []Waiter
}

func (e *Queued) Error() string {
	names := make([]string, len(e.Ahead))
	for i, w := range e.Ahead {
		names[i] = w.Owner
	}
	return fmt.Sprintf("%s is free but %d session(s) waited longer: %s",
		e.Resource, len(e.Ahead), strings.Join(names, ", "))
}

func (s Store) queuePath(resource string) string {
	return strings.TrimSuffix(s.path(resource), ".lease") + ".queue"
}

// readQueue returns every recorded waiter, live or not, oldest first. A missing
// file is an empty queue; so is a corrupt one, because a torn queue must not
// wedge the resource -- the lease file follows the same rule.
func (s Store) readQueue(qp string) ([]Waiter, error) {
	b, err := os.ReadFile(qp)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ws []Waiter
	if json.Unmarshal(b, &ws) != nil {
		return nil, nil
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Since.Before(ws[j].Since) })
	return ws, nil
}

func (s Store) writeQueue(qp string, ws []Waiter) error {
	if len(ws) == 0 {
		if err := s.rm(qp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	body, _ := json.MarshalIndent(ws, "", "  ") // strings and times cannot fail to marshal
	// ⛔ A PRIVATE temporary name. A fixed one is shared by any two writers the
	// lock fails to separate (a lock taken as stale from a stalled holder), and
	// the second rename then finds nothing: review of #53 lost 24 of 24
	// concurrent places that way.
	f, err := os.CreateTemp(filepath.Dir(qp), ".queue-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	rename := os.Rename
	if s.rename != nil {
		rename = s.rename
	}
	_, werr := f.Write(body)
	cerr := f.Close()
	// Renamed into place only when it is whole: a short write must not become
	// the line.
	if werr == nil && cerr == nil {
		werr = rename(tmp, qp)
	}
	return errors.Join(werr, cerr)
}

func live(ws []Waiter, now time.Time) []Waiter {
	var out []Waiter
	for _, w := range ws {
		if !now.After(w.Until) {
			out = append(out, w)
		}
	}
	return out
}

// Wait puts owner in line for resource, or refreshes its place, and keeps it
// until `until`. A waiter keeps its original Since however often it refreshes:
// that is what makes the line first come, first served. It returns how many
// live waiters are ahead.
func (s Store) Wait(resource, owner string, until time.Time) (int, error) {
	now := s.now()
	if err := s.place(resource, owner, until, now); err != nil {
		return 0, err
	}
	// Counted after the lock is released: the line spans other queue files,
	// which have their own locks, and a count is advice, not a grant.
	line, err := s.waitersAhead(resource, owner, now, true)
	return len(line), err
}

// place writes or refreshes owner's place in resource's own queue, under its lock.
func (s Store) place(resource, owner string, until, now time.Time) error {
	// A directory that cannot be made is reported by the lock, which is made in it.
	_ = os.MkdirAll(s.Dir, 0o755)
	qp := s.queuePath(resource)
	unlock, err := s.lock(qp)
	if err != nil {
		return err
	}
	defer unlock()
	ws, err := s.readQueue(qp)
	if err != nil {
		return err
	}
	ws = live(ws, now)
	found := false
	for i := range ws {
		if ws[i].Owner == owner {
			ws[i].Until = until
			if ws[i].Resource == "" {
				// A place without its name is invisible to every other spelling's
				// line; a refresh gives it back.
				ws[i].Resource = resource
			}
			found = true
		}
	}
	if !found {
		ws = append(ws, Waiter{Resource: resource, Owner: owner, Since: now, Until: until})
	}
	return s.writeQueue(qp, ws)
}

// Leave takes owner out of the line for resource, under every spelling of it
// and for everything it encloses. Leaving a line one is not in is not an error.
//
// ⛔ Every spelling (review of #53): a session that waited as
// go-gitsafe/gitsafe and won as gitsafe left only the second line, and its old
// place then held the next waiter back after it had released.
//
// ⛔ But NOT what encloses it (review of #53, round 3): a session waiting for
// the organisation go-x that claimed go-x/a lost its place for go-x, which
// getting go-x/a did not give it.
func (s Store) Leave(resource, owner string) error {
	own := s.queuePath(resource)
	if err := s.leaveIn(own, owner); err != nil {
		return err
	}
	key := Normalize(resource)
	for qp, ws := range s.otherQueues(own) {
		for _, w := range ws {
			if w.Owner != owner || w.Resource == "" {
				continue
			}
			if wk := Normalize(w.Resource); covers(key, wk) || SameRepository(key, wk) {
				// Best effort, as the own line's is not: that one is the line
				// the caller named.
				_ = s.leaveIn(qp, owner)
				break
			}
		}
	}
	return nil
}

func (s Store) leaveIn(qp, owner string) error {
	unlock, err := s.lock(qp)
	if err != nil {
		return err
	}
	defer unlock()
	ws, err := s.readQueue(qp)
	if err != nil {
		return err
	}
	now := s.now()
	var keep []Waiter
	for _, w := range live(ws, now) {
		if w.Owner != owner {
			keep = append(keep, w)
		}
	}
	return s.writeQueue(qp, keep)
}

// Queue returns the live waiters for resource's own line, longest-waiting first.
func (s Store) Queue(resource string) ([]Waiter, error) {
	ws, err := s.readQueue(s.queuePath(resource))
	if err != nil {
		return nil, err
	}
	return live(ws, s.now()), nil
}

// Line is everyone waiting for resource under any spelling that is one claim
// with it, longest-waiting first, each session once. It is what `claims` shows.
func (s Store) Line(resource string) ([]Waiter, error) {
	merged, err := s.merged(resource)
	if err != nil {
		return nil, err
	}
	return earliestPerOwner(live(merged, s.now())), nil
}

// Overlaps reports whether two resources are one claim under the lease rules:
// one encloses the other, or they name one repository differently.
func Overlaps(a, b string) bool { return overlaps(Normalize(a), Normalize(b)) }

// ahead is the waiters in ws that come before owner: all of them when owner is
// not in line, since a session that has not waited has not earned a place.
func ahead(ws []Waiter, owner string) []Waiter {
	for i, w := range ws {
		if w.Owner == owner {
			return ws[:i]
		}
	}
	return ws
}

// waitersAhead is the live line in front of owner for resource: its own queue
// and every other queue whose resource overlaps it (an enclosing organisation,
// the bare or dashed name of the same repository), merged by arrival. Without
// the merge, `claim gitsafe` walked past a line kept under go-gitsafe/gitsafe
// (review of #53) -- the spelling collision of 2026-11-22, back through the
// queue.
//
// The resource's OWN queue must be readable; another one that cannot be read
// is skipped, as overlapping skips a torn lease.
//
// ⛔ A waiter for ANOTHER resource counts only while it could be granted that
// resource now (review of #53, found by both reviewers): a session waiting for
// the organisation go-x, blocked by a live lease on go-x/b, otherwise froze
// every FREE repository of go-x for as long as go-x/b was held -- a waiter that
// cannot take what it waits for must not stop others taking what nobody holds.
//
// ⛔ That holds for a waiter for the SAME key too (review of #53, round 3): a
// lease of someone else's that blocks it would have refused this caller as Held
// before the line was read, so what still blocks it is the caller's own lease
// -- carol holding `gitsafe` was QUEUED, asking for go-gitsafe/gitsafe, behind
// bob, who waited only because of her.
//
// The price is that a waiter for an organisation is served best effort: claims
// on its free repositories keep being granted while one of them is held.
//
// position is for Wait's count, read while the resource is still held: nobody
// ahead can be granted it then, yet a waiter for the same key is still ahead.
func (s Store) waitersAhead(resource, owner string, now time.Time, position bool) ([]Waiter, error) {
	merged, err := s.merged(resource)
	if err != nil {
		return nil, err
	}
	key := Normalize(resource)
	var line []Waiter
	for _, w := range live(merged, now) {
		if w.Resource == "" || (position && Normalize(w.Resource) == key) || s.grantable(w, now) {
			line = append(line, w)
		}
	}
	return ahead(earliestPerOwner(line), owner), nil
}

// merged is resource's own queue, which must be readable, plus the waiters of
// every other queue whose resource overlaps it; another queue that cannot be
// read is skipped, as overlapping skips a torn lease.
func (s Store) merged(resource string) ([]Waiter, error) {
	own := s.queuePath(resource)
	merged, err := s.readQueue(own)
	if err != nil {
		return nil, err
	}
	key := Normalize(resource)
	for _, ws := range s.otherQueues(own) {
		for _, w := range ws {
			if w.Resource != "" && overlaps(Normalize(w.Resource), key) {
				merged = append(merged, w)
			}
		}
	}
	return merged, nil
}

// grantable reports whether w could be given its own resource now: no live
// lease of another session holds it, under its name or an overlapping one.
func (s Store) grantable(w Waiter, now time.Time) bool {
	p := s.path(w.Resource)
	if cur, err := s.read(p); err == nil && cur.Owner != w.Owner && !cur.Takeable(now, s.Idle) {
		return false
	}
	return s.overlapping(p, Normalize(w.Resource), w.Owner, now) == nil
}

// otherQueues reads every queue file in the store except skip. A directory that
// cannot be listed yields none: it is not a line.
func (s Store) otherQueues(skip string) map[string][]Waiter {
	out := map[string][]Waiter{}
	entries, _ := os.ReadDir(s.Dir) // on an error, none: an unlistable directory holds no line
	for _, e := range entries {
		qp := filepath.Join(s.Dir, e.Name())
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".queue") || qp == skip {
			continue
		}
		if ws, err := s.readQueue(qp); err == nil && len(ws) > 0 {
			out[qp] = ws
		}
	}
	return out
}

// earliestPerOwner sorts by arrival and keeps each owner once, at its earliest
// place: a session waiting under two spellings is one session in line.
func earliestPerOwner(ws []Waiter) []Waiter {
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Since.Before(ws[j].Since) })
	seen := map[string]bool{}
	var out []Waiter
	for _, w := range ws {
		if !seen[w.Owner] {
			seen[w.Owner] = true
			out = append(out, w)
		}
	}
	return out
}

// Lines returns every live line in the store, keyed by the resource its
// earliest waiter asked for. `claims` prints them, including the line for a
// resource nobody holds -- the one case where "why was I QUEUED" has no lease
// row to hang from.
func (s Store) Lines() map[string][]Waiter {
	out := map[string][]Waiter{}
	now := s.now()
	for _, ws := range s.otherQueues("") {
		if l := live(ws, now); len(l) > 0 && l[0].Resource != "" {
			out[l[0].Resource] = l
		}
	}
	return out
}
