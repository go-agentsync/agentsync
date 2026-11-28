package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	Owner string    `json:"owner"`
	Since time.Time `json:"since"`
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
	tmp := qp + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, qp)
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
	// A directory that cannot be made is reported by the lock, which is made in it.
	_ = os.MkdirAll(s.Dir, 0o755)
	qp := s.queuePath(resource)
	unlock, err := s.lock(qp)
	if err != nil {
		return 0, err
	}
	defer unlock()
	now := s.now()
	ws, err := s.readQueue(qp)
	if err != nil {
		return 0, err
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
		ws = append(ws, Waiter{Owner: owner, Since: now, Until: until})
	}
	if err := s.writeQueue(qp, ws); err != nil {
		return 0, err
	}
	return len(ahead(ws, owner)), nil
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

// waitersAhead is the live line in front of owner for resource.
func (s Store) waitersAhead(resource, owner string, now time.Time) ([]Waiter, error) {
	ws, err := s.readQueue(s.queuePath(resource))
	if err != nil {
		return nil, err
	}
	return ahead(live(ws, now), owner), nil
}
