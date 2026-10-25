package main

import (
	"bytes"
	"errors"
	"fmt"
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
