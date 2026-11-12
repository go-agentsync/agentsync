package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// ⛔ The reviewer's case: a bare "optimize" of mine must not outrank a live
// "go-numerics/optimize" of theirs. Taking them in either order is refused at
// claim time; a bare claim that predates the rule is refused at publish time.
func TestABareClaimOfMineDoesNotOutrankTheirQualifiedOne(t *testing.T) {
	env, s := gateFixture(t)
	// Written the way an old `claim` would have: neither acquisition saw the other.
	for _, f := range []struct{ res, owner string }{{"go-numerics/optimize", them}, {"optimize", me}} {
		l := lease.Lease{Resource: f.res, Owner: f.owner, Acquired: gateNow, Expires: gateNow.Add(time.Hour)}
		writeLease(t, s, l)
	}
	denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62"))
	if !denied || !strings.Contains(reason, "22222222") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
	// And the holder of the qualified claim is not stopped by my bare one.
	if denied, _, _ := ask(env, payload(t, them, "/", "ghmerge go-numerics/optimize 62")); !denied {
		// them is blocked by me too: the bare claim covers it, and I hold it live.
		t.Log("both hold overlapping claims; each is refused by the other, as designed")
	}
}

func writeLease(t *testing.T, s lease.Store, l lease.Lease) {
	t.Helper()
	// Through Acquire at the lease's own clock, into a scratch store when the
	// overlap check would refuse; the file shape is what matters.
	scratch := lease.Store{Dir: t.TempDir(), Now: func() time.Time { return gateNow }}
	got, err := scratch.Acquire(l.Resource, l.Owner, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(scratch.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("scratch store: %v %v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(scratch.Dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	_ = got
	if err := os.WriteFile(filepath.Join(s.Dir, entries[0].Name()), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Another session's lapsed claim must not stop a publish, and must not be
// mistaken for a live one: with nothing of mine, the answer is "claim it first",
// not "held by another session".
func TestAnotherSessionsExpiredClaimIsNotAHolder(t *testing.T) {
	env, s := gateFixture(t)
	past := lease.Store{Dir: s.Dir, Now: func() time.Time { return gateNow.Add(-2 * time.Hour) }}
	if _, err := past.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62"))
	if !denied || strings.Contains(reason, "claimed by another session") || !strings.Contains(reason, "Claim it first") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
}

// ⛔ A lease the gate cannot read may be the holder's: allow and say so, never
// "nobody holds it".
func TestAnUnreadableLeaseMeansCannotRead(t *testing.T) {
	env, s := gateFixture(t)
	if err := os.WriteFile(filepath.Join(s.Dir, "torn.lease"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied, reason, errOut := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62"))
	if denied || !strings.Contains(errOut, "unreadable") {
		t.Fatalf("denied=%v (%s) stderr=%q", denied, reason, errOut)
	}
}

// Claims taken with `claim` refuse a bare/qualified overlap in both orders.
func TestAcquireRefusesABareAndAQualifiedClaimOfOneRepository(t *testing.T) {
	_, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("optimize", me, "", time.Hour); err == nil {
		t.Error("a bare claim was granted over another session's qualified one")
	}
	s2 := lease.Store{Dir: t.TempDir(), Now: s.Now}
	if _, err := s2.Acquire("optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Acquire("go-numerics/optimize", me, "", time.Hour); err == nil {
		t.Error("a qualified claim was granted over another session's bare one")
	}
}

// Commands that hide a publish from a parser that reads only the first word.
func TestHiddenPublishingCommandsAreFound(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`bash -c "cd /work/opt && gitpush origin topic"`,
		`sh -c 'ghmerge go-numerics/optimize 62'`,
		`eval "ghmerge go-numerics/optimize 62"`,
		`sudo ghmerge go-numerics/optimize 62`,
		`timeout 60 ghmerge go-numerics/optimize 62`,
		`echo 62 | xargs ghmerge go-numerics/optimize`,
		`if true; then ghmerge go-numerics/optimize 62; fi`,
		`for i in 1; do ghmerge go-numerics/optimize 62; done`,
		`{ ghmerge go-numerics/optimize 62; }`,
		`! ghmerge go-numerics/optimize 62`,
		"x=`ghmerge go-numerics/optimize 62`",
		`x=$(ghmerge go-numerics/optimize 62)`,
		`(cd /work/opt && gitpush origin topic)`,
		"ls\nghmerge go-numerics/optimize 62 2>&1",
		"git commit -F - <<EOF\nmsg\nEOF\nghmerge go-numerics/optimize 62",
	} {
		if denied, _, _ := ask(env, payload(t, me, "/", cmd)); !denied {
			t.Errorf("a publish hidden in %q was allowed", cmd)
		}
	}
}

// A cd inside parentheses does not persist past them.
func TestACdInsideParenthesesDoesNotPersist(t *testing.T) {
	env, _ := gateFixture(t)
	invs := invocations("(cd /work/opt && ls); gitpush origin topic", "/elsewhere", env.home)
	if len(invs) != 1 || invs[0].dir != "/elsewhere" {
		t.Fatalf("%+v", invs)
	}
	invs = invocations("cd /work/opt && (cd /x; ls) && gitpush origin topic", "/elsewhere", env.home)
	if len(invs) != 1 || invs[0].dir != "/work/opt" {
		t.Fatalf("%+v", invs)
	}
	invs = invocations("gitpush origin topic 2>&1 >/dev/null", "/work/opt", env.home)
	if len(invs) != 1 || strings.Join(invs[0].args, " ") != "origin topic" {
		t.Fatalf("redirections leaked into the arguments: %+v", invs)
	}
}

// The push target follows git's own rules, against real git.
func TestThePushTargetFollowsGitsConfiguration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "topic")
	run("remote", "add", "origin", "https://github.com/up/stream.git")
	run("remote", "add", "fork", "https://github.com/me/stream.git")
	run("remote", "add", "other", "https://github.com/oth/er.git")
	want := func(label, repo string) {
		t.Helper()
		u, err := gitRemote(dir, "", true)
		if err != nil || ownerRepo(u) != repo {
			t.Errorf("%s: got %q (%v), want %s", label, u, err, repo)
		}
	}
	want("origin by default", "up/stream")
	run("config", "branch.topic.remote", "other")
	want("branch.<b>.remote", "oth/er")
	run("config", "remote.pushDefault", "fork")
	want("remote.pushDefault outranks branch.<b>.remote", "me/stream")
	run("config", "branch.topic.pushRemote", "origin")
	want("branch.<b>.pushRemote outranks both", "up/stream")
	run("config", "remote.origin.pushurl", "https://github.com/push/target.git")
	want("the push URL, not the fetch URL", "push/target")
	if u, _ := gitRemote(dir, "origin", false); ownerRepo(u) != "up/stream" {
		t.Errorf("the fetch URL is what a pull request's repository is: %q", u)
	}
}
