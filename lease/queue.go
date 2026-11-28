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
	line, err := s.waitersAhead(resource, owner, now)
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
			found = true
		}
	}
	if !found {
		ws = append(ws, Waiter{Resource: resource, Owner: owner, Since: now, Until: until})
	}
	return s.writeQueue(qp, ws)
}

// Leave takes owner out of the line for resource. Leaving a line one is not in
// is not an error.
func (s Store) Leave(resource, owner string) error {
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
	now := s.now()
	var keep []Waiter
	for _, w := range live(ws, now) {
		if w.Owner != owner {
			keep = append(keep, w)
		}
	}
	return s.writeQueue(qp, keep)
}

// Queue returns the live waiters for resource, longest-waiting first.
func (s Store) Queue(resource string) ([]Waiter, error) {
	ws, err := s.readQueue(s.queuePath(resource))
	if err != nil {
		return nil, err
	}
	return live(ws, s.now()), nil
}

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
func (s Store) waitersAhead(resource, owner string, now time.Time) ([]Waiter, error) {
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
	return ahead(earliestPerOwner(live(merged, now)), owner), nil
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
