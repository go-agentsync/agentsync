package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// clock is a fake whose sleep moves it. ⛔ A test for WAITING must not wait:
// with a real clock these cases would each take a lease's lifetime, and nobody
// runs a suite that costs minutes to tell you a retry loop retries.
type clock struct {
	t     time.Time
	slept []time.Duration
}

func (c *clock) now() time.Time { return c.t }

func (c *clock) sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
}

// waiting builds a store and a clock that share the same fake time, which is
// what lets a lease lapse without anything sleeping.
func waiting(t *testing.T, at time.Time) (lease.Store, *clock) {
	t.Helper()
	c := &clock{t: at}
	return lease.Store{Dir: t.TempDir(), Now: c.now}, c
}

func TestItWaitsOutTheHoldersLeaseAndThenGetsIt(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	// Somebody else holds it for a minute.
	if _, err := s.Acquire("go-pkgx/packages", "alice", "32-bit targets", time.Minute); err != nil {
		t.Fatalf("the holder could not take it: %v", err)
	}

	var out bytes.Buffer
	l, err := waitAcquire(s, "go-pkgx/packages", "bob", "mine now", time.Hour,
		5*time.Minute, 15*time.Second, c.now, c.sleep, &out)
	if err != nil {
		t.Fatalf("waitAcquire: %v\n%s", err, out.String())
	}
	if l.Owner != "bob" {
		t.Errorf("owner %q, want bob", l.Owner)
	}
	// ⛔ The substantive claim is that it waited the HOLDER'S LEASE OUT, not
	// that it slept a particular number of times. A fixed count here would
	// pin the test to whether a lease expiring exactly at `now` counts as
	// held -- a boundary that belongs to the store, is tested there, and has
	// nothing to do with whether retrying works.
	if !c.t.After(time.Unix(1000, 0).Add(time.Minute)) && !c.t.Equal(time.Unix(1000, 0).Add(time.Minute)) {
		t.Errorf("acquired at %s, before the holder's lease ran out at %s",
			c.t.UTC(), time.Unix(1000, 0).Add(time.Minute).UTC())
	}
	if len(c.slept) == 0 {
		t.Error("acquired without ever retrying, so nothing about waiting was exercised")
	}

	said := out.String()
	// The attempt it reports must be the one it actually made.
	if want := fmt.Sprintf("got it on attempt %d", len(c.slept)+1); !strings.Contains(said, want) {
		t.Errorf("it never said %q, having slept %d time(s):\n%s", want, len(c.slept), said)
	}
	// ⛔ The holder and their note are said ONCE, not on every retry: a line
	// repeated four times is a line the reader learns to skip.
	if n := strings.Count(said, "WAITING"); n != 1 {
		t.Errorf("announced the wait %d times, want 1:\n%s", n, said)
	}
	for _, want := range []string{"alice", "32-bit targets"} {
		if !strings.Contains(said, want) {
			t.Errorf("it never said %q:\n%s", want, said)
		}
	}
}

// ⛔ The deadline must return the SAME error the holder's refusal carries, so
// the caller prints the familiar REFUSED block. A session that waited and
// still did not get it is in exactly the state it would have been in at once,
// and should read the same.
func TestTheDeadlineReturnsTheHoldersRefusal(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-tex/engine", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	_, err := waitAcquire(s, "go-tex/engine", "bob", "", time.Hour,
		time.Minute, 15*time.Second, c.now, c.sleep, &out)
	var held *lease.Held
	if !errors.As(err, &held) {
		t.Fatalf("error %v, want a *lease.Held", err)
	}
	if held.By.Owner != "alice" {
		t.Errorf("refused by %q, want alice", held.By.Owner)
	}
	// It must not sleep past the deadline it was given.
	total := time.Duration(0)
	for _, d := range c.slept {
		total += d
	}
	if total > time.Minute {
		t.Errorf("slept %s in total, past the %s deadline", total, time.Minute)
	}
}

// ⛔ "The store would not answer" is not "somebody holds it". Waiting on it
// would spend the whole deadline reporting nothing, so it comes straight back
// -- and the proof is that it never slept.
func TestAStoreThatCannotAnswerIsNotWaitedOn(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	// A directory that cannot be written to: Acquire fails, and not with Held.
	s := lease.Store{Dir: "/nonexistent/under/a/file/that/is/not/there", Now: c.now}

	var out bytes.Buffer
	_, err := waitAcquire(s, "go-pkgx/packages", "bob", "", time.Hour,
		time.Hour, time.Second, c.now, c.sleep, &out)
	if err == nil {
		t.Fatal("an unusable store was treated as success")
	}
	var held *lease.Held
	if errors.As(err, &held) {
		t.Fatalf("an unusable store was reported as a held lease: %v", err)
	}
	if len(c.slept) != 0 {
		t.Errorf("slept %v while the store was unusable; it should come straight back", c.slept)
	}
}

// A free resource is taken on the first attempt, and says nothing about
// waiting -- the flag must cost nothing when there is nobody to wait for.
func TestNothingToWaitForIsSilent(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	var out bytes.Buffer
	if _, err := waitAcquire(s, "go-fft/fft", "bob", "", time.Hour,
		time.Hour, time.Second, c.now, c.sleep, &out); err != nil {
		t.Fatalf("waitAcquire: %v", err)
	}
	if len(c.slept) != 0 {
		t.Errorf("slept %v with nobody holding it", c.slept)
	}
	if out.Len() != 0 {
		t.Errorf("said %q with nobody holding it", out.String())
	}
}

// ⛔ The race that lost go-fleettools/fleettools nine times running
// (2026-11-27/28): bob's 9-minute wait ends while alice still holds the lease,
// alice's lease lapses, a newcomer asks first. Before the queue the newcomer
// won; now bob's place survives his deadline, the newcomer is QUEUED, and bob's
// next wait takes it on its first attempt.
func TestAWaiterKeepsItsTurnAcrossItsOwnDeadline(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", 70*time.Second); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := waitAcquire(s, "o/r", "bob", "", time.Hour, time.Minute, 15*time.Second, c.now, c.sleep, &out)
	var held *lease.Held
	if !errors.As(err, &held) {
		t.Fatalf("bob's first wait ended with %v, want Held by alice", err)
	}
	if !strings.Contains(out.String(), "0 session(s) ahead of you") {
		t.Errorf("the wait did not say bob's place in line:\n%s", out.String())
	}

	c.t = c.t.Add(15 * time.Second) // alice's lease has lapsed; bob is between waits
	_, err = s.Acquire("o/r", "dave", "", time.Hour)
	var queued *lease.Queued
	if !errors.As(err, &queued) || queued.Ahead[0].Owner != "bob" {
		t.Fatalf("dave, arriving first, got %v; want QUEUED behind bob", err)
	}

	c.slept = nil
	l, err := waitAcquire(s, "o/r", "bob", "", time.Hour, time.Minute, 15*time.Second, c.now, c.sleep, &out)
	if err != nil || l.Owner != "bob" {
		t.Fatalf("bob's second wait: %+v, %v", l, err)
	}
	if len(c.slept) != 0 {
		t.Errorf("bob slept %v: first in line, he should get it on the first attempt", c.slept)
	}
}

// A waiter whose session has gone leaves the line at once, rather than make
// the next one wait for its place to lapse.
func TestAGoneWaiterLeavesTheLine(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	calls := 0
	gone := errors.New("gone")
	var out bytes.Buffer
	_, err := waitAcquireFor(s, "o/r", "bob", "", "", time.Hour, time.Hour, 15*time.Second, c.now, c.sleep, &out,
		func() error {
			calls++
			if calls > 1 {
				return gone
			}
			return nil
		})
	if !errors.Is(err, gone) {
		t.Fatalf("got %v, want the requester's error", err)
	}
	if q, err := s.Queue("o/r"); err != nil || len(q) != 0 {
		t.Errorf("line after bob went = %+v, %v; want empty", q, err)
	}
}

// ⛔ A wait that cannot take its place in line is not waiting its turn: it
// would poll for the whole deadline with nobody keeping its place, which is the
// lottery this replaced. It comes straight back with the reason.
func TestAWaitThatCannotTakeItsPlaceSaysSo(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("solo", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	// Taking a place fails while the lease itself still answers Held.
	saved := waitPlace
	waitPlace = func(lease.Store, string, string, time.Time) (int, error) { return 0, errors.New("injected") }
	t.Cleanup(func() { waitPlace = saved })
	var out bytes.Buffer
	_, err := waitAcquire(s, "solo", "bob", "", time.Hour, time.Hour, 15*time.Second, c.now, c.sleep, &out)
	var held *lease.Held
	if err == nil || errors.As(err, &held) {
		t.Fatalf("got %v, want the failure to take a place", err)
	}
	if len(c.slept) != 0 {
		t.Errorf("slept %v without a place in line", c.slept)
	}
}

// agentsync#42's acceptance: waiters A then B, and a C that never waited
// claiming right at the lapse. A gets it, B is next, C is refused with the line.
func TestAClaimAtTheLapseIsRefusedWithTheLine(t *testing.T) {
	sandbox(t, "alice-1111")
	s := store()
	if _, err := s.Acquire("o/r", "holder-0000", "", -time.Second); err != nil { // already lapsed
		t.Fatal(err)
	}
	mustPlace(t, s, "o/r", "alice-1111")
	mustPlace(t, s, "o/r", "bob-2222")

	t.Setenv("AGENTSYNC_OWNER", "carol-3333")
	var err error
	out := capture(t, func() { err = claim([]string{"o/r"}) })
	if err == nil {
		t.Fatalf("carol, who never waited, took the lease from the line:\n%s", out)
	}
	for _, want := range []string{"QUEUED   o/r", "2 session(s) waited longer", "alice-11", "bob-2222", "claim --wait"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "alice-11") > strings.Index(out, "bob-2222") {
		t.Errorf("the line is not printed in order:\n%s", out)
	}

	t.Setenv("AGENTSYNC_OWNER", "alice-1111")
	if err := claim([]string{"o/r"}); err != nil {
		t.Fatalf("alice, first in line: %v", err)
	}
	out = capture(t, func() { err = claims(nil) })
	if err != nil || !strings.Contains(out, "in line: bob-2222 since") {
		t.Errorf("claims does not show bob next (%v):\n%s", err, out)
	}
}

func TestClaimsSaysWhenTheLineCannotBeRead(t *testing.T) {
	sandbox(t, "alice-1111")
	s := store()
	if _, err := s.Acquire("solo", "alice-1111", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir, "solo.queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	out := capture(t, func() { err = claims(nil) })
	if err != nil || !strings.Contains(out, "in line: unreadable") {
		t.Errorf("an unreadable line was not reported (%v):\n%s", err, out)
	}
}

// A wait that begins on a FREE lease another session is ahead for says so,
// rather than naming a holder there is none of.
func TestAWaitBehindTheLineSaysItIsNotYourTurn(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Wait("o/r", "alice", c.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := waitAcquire(s, "o/r", "bob", "", time.Hour, time.Minute, 15*time.Second, c.now, c.sleep, &out)
	var queued *lease.Queued
	if !errors.As(err, &queued) {
		t.Fatalf("got %v, want QUEUED behind alice", err)
	}
	for _, want := range []string{"free, but it is not your turn", "1 session(s) ahead of you"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the wait does not say %q:\n%s", want, out.String())
		}
	}
}

func mustPlace(t *testing.T, s lease.Store, resource, owner string) {
	t.Helper()
	if _, err := s.Wait(resource, owner, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// A place lives three retries, never under a minute -- and never more than a
// minute past the wait's own deadline.
func TestAPlaceLivesThreeRetriesButNotPastTheWait(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		every, wait, want time.Duration // want is from now
	}{
		{time.Second, time.Hour, time.Minute},
		{15 * time.Second, time.Hour, time.Minute},
		{20 * time.Second, time.Hour, time.Minute},
		{21 * time.Second, time.Hour, 63 * time.Second},
		{time.Minute, time.Hour, 3 * time.Minute},
		{time.Hour, time.Minute, 2 * time.Minute},         // capped: deadline + 1m
		{time.Minute, 30 * time.Second, 90 * time.Second}, // capped
		{time.Hour, -time.Minute, time.Minute},            // a deadline already past counts as now
	} {
		if got := placeUntil(now, now.Add(tc.wait), tc.every).Sub(now); got != tc.want {
			t.Errorf("every %s, wait %s: place lives %s, want %s", tc.every, tc.wait, got, tc.want)
		}
	}
}

// ⛔ Review of #53, CONFIRMED: with --every longer than --wait, one attempt
// wrote a place of three hours and returned; the resource, once free, refused
// every claim for those hours behind a session that had stopped waiting.
func TestALongIntervalDoesNotHoldTheLineForHours(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("o/r", "alice", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waitAcquire(s, "o/r", "bob", "", time.Hour, time.Minute, time.Hour, c.now, c.sleep, &out); err == nil {
		t.Fatal("bob got a lease alice holds")
	}
	c.t = c.t.Add(3 * time.Minute)
	if _, err := s.Acquire("o/r", "carol", "", time.Minute); err != nil {
		t.Fatalf("3 minutes after bob's 1-minute wait ended, carol is still refused: %v", err)
	}
}

func TestClaimsShowsTheLineForAFreeResource(t *testing.T) {
	sandbox(t, "carol-3333")
	mustPlace(t, store(), "go-x/free", "bob-2222")
	var err error
	out := capture(t, func() { err = claims(nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"free     go-x/free", "nobody holds it", "in line: bob-2222 since"} {
		if !strings.Contains(out, want) {
			t.Errorf("claims does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nothing is held") || strings.Contains(out, "no resource is claimed") {
		t.Errorf("claims called a resource with a line unclaimed:\n%s", out)
	}
}

// ⛔ --wait with several resources is REFUSED rather than given semantics
// nobody chose: it would acquire them as they free and block on the rest,
// leaving the session holding part of what it asked for while still unable to
// start. These checks run before the store is touched, so this test does not
// reach the session's real lock directory.
func TestWaitTakesExactlyOneResource(t *testing.T) {
	err := claim([]string{"--wait", "1m", "go-pkgx/packages", "go-tex/engine"})
	if err == nil {
		t.Fatal("--wait accepted two resources")
	}
	if !strings.Contains(err.Error(), "exactly one resource") {
		t.Errorf("error %q does not say why", err)
	}
}

func TestAZeroIntervalIsRefused(t *testing.T) {
	err := claim([]string{"--wait", "1m", "--every", "0s", "go-pkgx/packages"})
	if err == nil {
		t.Fatal("--every 0 accepted: that is a busy loop, not a wait")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error %q does not say why", err)
	}
}
