package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// swapStdio points os.Stdin at in and captures what is written to os.Stdout.
func swapStdio(t *testing.T, in string) func() string {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inW.WriteString(in)
	inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdin, os.Stdout, os.Stderr = inR, outW, devnull
	var restored bool
	restore := func() {
		if !restored {
			restored = true
			os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
			outW.Close()
			devnull.Close()
		}
	}
	t.Cleanup(restore)
	return func() string {
		restore()
		var b bytes.Buffer
		b.ReadFrom(outR)
		return b.String()
	}
}

// `gate --hook` is wired to gateHook, and answers 0 with the decision in JSON.
func TestGateHookFlagAnswersThroughTheEntryPoint(t *testing.T) {
	sandbox(t, me)
	if err := os.MkdirAll(store().Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": me, "cwd": "/work/x", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "ghmerge go-numerics/optimize 62"},
	})
	read := swapStdio(t, string(b))
	if err := gate([]string{"--hook"}); err != nil {
		t.Fatalf("gate --hook must always succeed: %v", err)
	}
	out := read()
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "go-numerics/optimize") {
		t.Fatalf("the hook did not answer through gate: %q", out)
	}
}

func TestGateWithNoArgumentsIsUsage(t *testing.T) {
	sandbox(t, me)
	err := gate(nil)
	if err == nil || err.Error() != gateUsage {
		t.Fatalf("got %v, want the usage", err)
	}
}

// Resources named on the command line: one held by this session is allowed, one
// nobody holds is refused, and any refusal makes the whole call unavailable.
func TestGateResourcesOnTheCommandLine(t *testing.T) {
	sandbox(t, me)
	if _, err := store().Acquire("go-numerics/optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	read := swapStdio(t, "")
	if err := gate([]string{"go-numerics/optimize"}); err != nil {
		t.Fatalf("held by this session, yet: %v", err)
	}
	if out := read(); out != "allowed  go-numerics/optimize\n" {
		t.Fatalf("allowed line: %q", out)
	}

	read = swapStdio(t, "")
	err := gate([]string{"go-numerics/optimize", "o/unheld"})
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("a refused resource must make the call unavailable, got %v", err)
	}
	out := read()
	if !strings.Contains(out, "allowed  go-numerics/optimize") || !strings.Contains(out, "no session holds o/unheld") {
		t.Fatalf("both verdicts must be printed: %q", out)
	}
}

// holder reads nothing from a store that is missing or unreadable: nobody holds.
func TestHolderOfAMissingOrTornStoreIsNil(t *testing.T) {
	env, s := gateFixture(t)
	env.owner = me
	if _, err := s.Acquire("o/r", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if h := env.holder("o/r"); h == nil || h.Owner != them {
		t.Fatalf("control: the other session's lease must be found, got %v", h)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "torn.lease"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := env.holder("o/r"); h != nil {
		t.Fatalf("a torn lease is not a verdict, got %v", h)
	}
	gone := env
	gone.store = lease.Store{Dir: filepath.Join(t.TempDir(), "absent")}
	if h := gone.holder("o/r"); h != nil {
		t.Fatalf("a missing directory has no holder, got %v", h)
	}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	notDir := env
	notDir.store = lease.Store{Dir: file}
	if h := notDir.holder("o/r"); h != nil {
		t.Fatalf("a file is not a claims directory, got %v", h)
	}
}

func TestCdWithFlagsFollowsTheDirectory(t *testing.T) {
	invs := invocations("cd -P /work/opt && ghmerge 1", "/elsewhere", "/home/u")
	if len(invs) != 1 || invs[0].dir != "/work/opt" {
		t.Fatalf("cd -P must be skipped to reach its directory: %+v", invs)
	}
}

func TestStripCommentsKeepsAnEscapedQuoteInsideQuotes(t *testing.T) {
	in := "echo \"a\\\" # not a comment\" # real\nghmerge o/r 1"
	got := stripComments(in)
	want := "echo \"a\\\" # not a comment\" \nghmerge o/r 1"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// A backslash inside single quotes is literal: it does not swallow the quote.
	if got := stripComments("echo 'a\\' # c"); got != "echo 'a\\' " {
		t.Fatalf("single quotes: %q", got)
	}
}

func TestResolveDir(t *testing.T) {
	for _, c := range []struct{ cur, to, want string }{
		{"/c", "~", "/home/u"},
		{"/c", "~/x", "/home/u/x"},
		{"/c", "$HOME", "/home/u"},
		{"/c", "$HOME/x", "/home/u/x"},
		{"/c", "$ELSEWHERE/x", "/c"},
		{"/c", "/abs/../y", "/y"},
		{"", "rel", "rel"},
		{"/c", "rel", "/c/rel"},
	} {
		if got := resolveDir(c.cur, c.to, "/home/u"); got != c.want {
			t.Errorf("resolveDir(%q, %q) = %q, want %q", c.cur, c.to, got, c.want)
		}
	}
}

func TestTokensKeepsAnEscapedCharacterInsideDoubleQuotes(t *testing.T) {
	ws := tokens(`echo "a\"b"`)
	if len(ws) != 2 || ws[1].s != `a"b` || !ws[1].quoted {
		t.Fatalf("got %+v", ws)
	}
}

func TestTargetReadsRepoEqualsAndNamesWhatItCannotRead(t *testing.T) {
	env, _ := gateFixture(t)
	if r, err := env.target(invocation{prog: "ghmerge", args: []string{"--repo=go-numerics/optimize", "62"}}); err != nil || r != "go-numerics/optimize" {
		t.Fatalf("--repo=: %q %v", r, err)
	}
	if r, err := env.target(invocation{prog: "ghmerge", args: []string{"-repo=go-numerics/optimize"}}); err != nil || r != "go-numerics/optimize" {
		t.Fatalf("-repo=: %q %v", r, err)
	}
	if _, err := env.target(invocation{prog: "ghmerge", args: []string{"--repo=junk"}}); err == nil || !strings.Contains(err.Error(), `cannot read a repository from "junk"`) {
		t.Fatalf("an explicit repo that is none: %v", err)
	}
	if _, err := env.target(invocation{prog: "ghmerge", args: []string{"62"}}); err == nil || !strings.Contains(err.Error(), "no repository named and no working directory") {
		t.Fatalf("no dir: %v", err)
	}
	env.remote = func(string, string, bool) (string, error) { return "/some/local/path", nil }
	if _, err := env.target(invocation{prog: "ghmerge", args: []string{"62"}, dir: "/work/opt"}); err == nil || !strings.Contains(err.Error(), `is "/some/local/path", not an owner/repo`) {
		t.Fatalf("a remote that is no owner/repo: %v", err)
	}
}

func TestOwnerRepoOfAURLWithoutARepository(t *testing.T) {
	if got := ownerRepo("https://github.com/solo"); got != "" {
		t.Fatalf("an owner alone is no repository, got %q", got)
	}
	if got := ownerRepo("https://github.com/go-numerics/optimize/pull/3"); got != "go-numerics/optimize" {
		t.Fatalf("control: %q", got)
	}
}

func TestGitRemoteRefusesANameThatIsAFlag(t *testing.T) {
	if u, err := gitRemote(t.TempDir(), "--upload-pack=x", false); err == nil || u != "" || err.Error() != "not a remote name" {
		t.Fatalf("got %q %v", u, err)
	}
}

// ---- gate --post ----

func TestEnvDurationRejectsWhatIsNotADuration(t *testing.T) {
	t.Setenv("AGENTSYNC_COV43", "soon")
	if d := envDuration("AGENTSYNC_COV43", 7*time.Minute); d != 7*time.Minute {
		t.Fatalf("the default must be used, got %s", d)
	}
	t.Setenv("AGENTSYNC_COV43", "3m")
	if d := envDuration("AGENTSYNC_COV43", 7*time.Minute); d != 3*time.Minute {
		t.Fatalf("control: %s", d)
	}
}

func TestGatePostReleasesNothingOnAnUnreadablePayload(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	gatePost(strings.NewReader("{not json"), &out, &errOut, env)
	if out.Len() != 0 || !strings.Contains(errOut.String(), "could not read the hook payload; releasing nothing") {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
	if ls, _ := s.List(); len(ls) != 1 {
		t.Fatalf("a lease was released on a payload nobody could read: %v", ls)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestGatePostReleasesNothingWhenStdinFails(t *testing.T) {
	env, _ := gateFixture(t)
	var out, errOut bytes.Buffer
	gatePost(failingReader{}, &out, &errOut, env)
	if out.Len() != 0 || !strings.Contains(errOut.String(), "releasing nothing") {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}

func TestGatePostSaysWhyItCouldNotNameARepositoryAndReleasesOnceEach(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	cmd := "ghmerge --repo=junk 1; ghmerge go-numerics/optimize 62; ghmerge go-numerics/optimize 63"
	_, errOut := post(t, env, me, "/work/opt", cmd)
	if !strings.Contains(errOut, `ghmerge: cannot read a repository from "junk"; releasing nothing`) {
		t.Fatalf("the unreadable target is not reported: %q", errOut)
	}
	if n := strings.Count(errOut, "is released."); n != 1 {
		t.Fatalf("the repository must be released and announced once, got %d:\n%s", n, errOut)
	}
	if ls, _ := s.List(); len(ls) != 0 {
		t.Fatalf("the lease survived: %v", ls)
	}
}

// A claims directory that can be read but not written to: the release and the
// activity record both fail, and both say so rather than pretend.
func readOnlyLeases(t *testing.T) (gateEnv, lease.Store) {
	t.Helper()
	env, s := gateFixture(t)
	env.owner = me
	if _, err := s.Acquire("go-numerics/optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.Dir, 0o755) })
	if f, err := os.OpenFile(filepath.Join(s.Dir, "probe"), os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced here (running as root?)")
	}
	return env, s
}

func TestFinishReportsALeaseItCouldNotRelease(t *testing.T) {
	env, _ := readOnlyLeases(t)
	said := env.finish("go-numerics/optimize", "ghmerge")
	if len(said) != 1 || !strings.Contains(said[0], "ghmerge succeeded but the lease on go-numerics/optimize could not be released:") {
		t.Fatalf("got %q", said)
	}
}

func TestTouchReportsActivityItCouldNotRecord(t *testing.T) {
	env, _ := readOnlyLeases(t)
	var errOut bytes.Buffer
	env.touch("go-numerics/optimize", &errOut)
	if !strings.Contains(errOut.String(), "agentsync gate: could not record activity on go-numerics/optimize:") {
		t.Fatalf("got %q", errOut.String())
	}
}
