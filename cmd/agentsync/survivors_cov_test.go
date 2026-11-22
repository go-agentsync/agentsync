package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each test here kills a mutant that survived the first sweep over #43's
// coverage: a guard whose deletion the suite did not notice because the
// failure it prevents was reached by a second path with a different message.

func TestMemoryIndexFollowsTheTranscriptNotTheWorkingDirectory(t *testing.T) {
	const sid = "abc-session"
	t.Setenv("AGENTSYNC_MEMORY", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	h := t.TempDir()
	fakeHome(t, h)
	setVar(t, &getwd, func() (string, error) { return "/somewhere/else", nil })
	dir := filepath.Join(h, ".claude", "projects", "-the-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := memoryIndex(), filepath.Join(dir, "memory", "MEMORY.md"); got != want {
		t.Errorf("memoryIndex() = %q, want %q", got, want)
	}
}

func TestSessionProjectDirNamesABadPatternNotAMissingTranscript(t *testing.T) {
	_, err := sessionProjectDir("[", "abc")
	if !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("got %v, want the pattern error", err)
	}
}

func TestRecordFailuresNameTheirOwnCause(t *testing.T) {
	t.Run("unreadable is not a JSON error", func(t *testing.T) {
		sandbox(t, "me-1")
		if err := os.MkdirAll(recordPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := loadRecords()
		var syn *json.SyntaxError
		if err == nil || errors.As(err, &syn) || strings.Contains(err.Error(), "unexpected end") {
			t.Errorf("got %v, want the read failure itself", err)
		}
	})
	t.Run("the directory error is the one reported", func(t *testing.T) {
		cov43FileForStore(t, "me-1")
		err := saveRecords([]record{{Name: "x"}})
		if err == nil || !strings.Contains(err.Error(), "mkdir") {
			t.Errorf("got %v, want the mkdir failure", err)
		}
	})
}

func TestLineFromFrontmatterSaysTheFileIsNotReadable(t *testing.T) {
	_, err := lineFromFrontmatter(filepath.Join(t.TempDir(), "absent.md"), "absent")
	if !isUsage(err) || !strings.Contains(err.Error(), "absent.md is not readable") {
		t.Errorf("got %v", err)
	}
}

func TestMemAddRefusesTheWrongNumberOfArguments(t *testing.T) {
	_, index := memEnv(t)
	before := indexText(t, index)
	for _, args := range [][]string{
		{},
		{"m", "- [m](m.md): d", "extra"},
	} {
		err := memAdd(args)
		if !isUsage(err) || !strings.Contains(err.Error(), "usage: agentsync mem-add [--section H]") {
			t.Errorf("memAdd(%q) = %v, want the usage refusal", args, err)
		}
	}
	if indexText(t, index) != before {
		t.Error("the index was edited by a call that was refused")
	}
}

func TestMemAddReportsTheWriteFailureItself(t *testing.T) {
	cov43NeedChmod(t)
	_, index := memEnv(t)
	if err := os.Chmod(index, 0o400); err != nil {
		t.Fatal(err)
	}
	err := memAdd([]string{"m", "- [m](m.md): d"})
	if err == nil || strings.Contains(err.Error(), "another writer") {
		t.Errorf("got %v, want the permission failure, not a guess about another writer", err)
	}
}

func TestMemAddDoesNotOverwriteRecordsItCouldNotRead(t *testing.T) {
	memEnv(t)
	if err := os.MkdirAll(filepath.Dir(recordPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	const torn = "{not json"
	if err := os.WriteFile(recordPath(), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := memAdd([]string{"m", "- [m](m.md): d"}); err == nil {
		t.Fatal("torn records were treated as none")
	}
	if b, _ := os.ReadFile(recordPath()); string(b) != torn {
		t.Errorf("the records file was replaced by %q", b)
	}
}

func TestClaimsOnAnEmptyStoreSaysNothingIsClaimed(t *testing.T) {
	sandbox(t, "me-1111")
	out := said(t, func() error { return claims(nil) })
	if !strings.Contains(out, "no resource is claimed") || strings.Contains(out, "nothing is held right now") {
		t.Errorf("output: %q", out)
	}
}

func TestResolveDirKeepsARelativePathAsWritten(t *testing.T) {
	if got := resolveDir("", "./rel", "/home/u"); got != "./rel" {
		t.Errorf("resolveDir with no current directory = %q, want ./rel", got)
	}
}

func TestGateSaysNothingAboutAnOwnerWhenNothingPublishes(t *testing.T) {
	env, _ := gateFixture(t)
	env.override, env.session = "", ""
	var out, errOut bytes.Buffer
	gateHook(payload(t, "", "/work/opt", "ls -l"), &out, &errOut, env)
	if errOut.Len() != 0 || out.Len() != 0 {
		t.Errorf("a command that publishes nothing was answered: out=%q err=%q", out.String(), errOut.String())
	}
	errOut.Reset()
	gateHook(payload(t, "", "/work/opt", "ghmerge 1"), &out, &errOut, env)
	if !strings.Contains(errOut.String(), "cannot tell which session this is") {
		t.Errorf("a publishing command with no owner said %q", errOut.String())
	}
}

func TestAQuotedWordIsTheCommandNotAnAssignment(t *testing.T) {
	if invs := invocations(`"X=1" ghmerge 1`, "/work/opt", "/home/u"); len(invs) != 0 {
		t.Errorf("a quoted word named a command, and ghmerge after it is its argument: %+v", invs)
	}
	if invs := invocations(`X=1 ghmerge 1`, "/work/opt", "/home/u"); len(invs) != 1 {
		t.Errorf("control: an unquoted assignment is skipped: %+v", invs)
	}
}

func TestTargetRefusesBackfillOnlyForGhrelease(t *testing.T) {
	env, _ := gateFixture(t)
	_, err := env.target(invocation{prog: "ghrelease", args: []string{"backfill"}, dir: "/work/opt"})
	if err == nil || !strings.Contains(err.Error(), "backfill spans many repositories") {
		t.Errorf("ghrelease backfill: got %v", err)
	}
	got, err := env.target(invocation{prog: "ghissue", args: []string{"backfill"}, dir: "/work/opt"})
	if err != nil || got != "go-numerics/optimize" {
		t.Errorf("another program's argument named backfill: got %q, %v", got, err)
	}
}

func TestTargetReportsWhyTheRemoteCouldNotBeRead(t *testing.T) {
	env, _ := gateFixture(t)
	_, err := env.target(invocation{prog: "ghmerge", args: []string{"1"}, dir: "/work/nowhere"})
	if err == nil || !strings.Contains(err.Error(), "no such remote") {
		t.Errorf("got %v, want the remote's own failure", err)
	}
}
