package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cov43Read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFoldRefusesStrayArguments(t *testing.T) {
	anIndexWithALandingBlock(t)
	err := memFold([]string{"--hub", "hub", "extra"})
	if err == nil || !strings.Contains(err.Error(), "usage: agentsync mem-fold") {
		t.Fatalf("got %v, want the usage refusal", err)
	}
}

func TestFoldRefusesAnEmptyIndexPath(t *testing.T) {
	anIndexWithALandingBlock(t)
	err := memFold([]string{"--hub", "hub", "--index", ""})
	if err == nil || !strings.Contains(err.Error(), "no memory index for this directory") {
		t.Fatalf("got %v, want the no-index refusal", err)
	}
}

func TestFoldRefusesAHubThatDoesNotExist(t *testing.T) {
	dir, index := anIndexWithALandingBlock(t)
	before := cov43Read(t, index)
	err := memFold([]string{"--hub", "nohub"})
	if err == nil || !strings.Contains(err.Error(), "hub nohub.md") {
		t.Fatalf("got %v, want the missing-hub refusal", err)
	}
	if cov43Read(t, index) != before {
		t.Error("the index changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "nohub.md")); err == nil {
		t.Error("a hub was created")
	}
}

func TestFoldReportsAnIndexItCannotRead(t *testing.T) {
	dir, _ := anIndexWithALandingBlock(t)
	err := memFold([]string{"--hub", "hub", "--index", filepath.Join(dir, "ABSENT.md")})
	if err == nil || !strings.Contains(err.Error(), "ABSENT.md") {
		t.Fatalf("got %v, want the read error", err)
	}
}

func TestFoldRefusesASectionThatIsNotThere(t *testing.T) {
	_, index := anIndexWithALandingBlock(t)
	before := cov43Read(t, index)
	err := memFold([]string{"--hub", "hub", "--section", "Nope"})
	if err == nil || !strings.Contains(err.Error(), `no section matching "Nope"`) {
		t.Fatalf("got %v, want the no-section refusal", err)
	}
	if cov43Read(t, index) != before {
		t.Error("the index changed")
	}
}

func TestFoldReportsADirectoryItCannotList(t *testing.T) {
	cov43NeedChmod(t)
	dir, index := anIndexWithALandingBlock(t)
	before := cov43Read(t, index)
	// Search but no read permission: the stat and the index read succeed, the
	// listing does not.
	if err := os.Chmod(dir, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := memFold([]string{"--hub", "hub"})
	if err == nil || !strings.Contains(err.Error(), "reading "+dir) {
		t.Fatalf("got %v, want the listing error", err)
	}
	_ = os.Chmod(dir, 0o700)
	if cov43Read(t, index) != before {
		t.Error("the index changed")
	}
}

func TestFoldReportsAHubItCannotRead(t *testing.T) {
	cov43NeedChmod(t)
	dir, index := anIndexWithALandingBlock(t)
	before := cov43Read(t, index)
	hub := filepath.Join(dir, "hub.md")
	// Write-only: reading fails, but writing would succeed, so a fold that
	// went on after the failed read would report success.
	if err := os.Chmod(hub, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hub, 0o600) })
	err := memFold([]string{"--hub", "hub"})
	if err == nil || !strings.Contains(err.Error(), "hub.md") {
		t.Fatalf("got %v, want the hub read error", err)
	}
	_ = os.Chmod(hub, 0o600)
	if got := cov43Read(t, hub); got != "Rows folded out of the index land here.\n" {
		t.Errorf("the hub was rewritten:\n%s", got)
	}
	if cov43Read(t, index) != before {
		t.Error("the index changed")
	}
}

func TestFoldReportsAHubItCannotWrite(t *testing.T) {
	cov43NeedChmod(t)
	dir, index := anIndexWithALandingBlock(t)
	before := cov43Read(t, index)
	hub := filepath.Join(dir, "hub.md")
	if err := os.Chmod(hub, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hub, 0o600) })
	err := memFold([]string{"--hub", "hub"})
	if err == nil || !strings.Contains(err.Error(), "hub.md") {
		t.Fatalf("got %v, want the hub write error", err)
	}
	if cov43Read(t, index) != before {
		t.Error("the index changed")
	}
}

// The index cannot be written after the hub already was: the hub must be put
// back, or the rows would live in two places and the hub would claim a fold
// that never happened.
func TestFoldPutsTheHubBackWhenTheIndexCannotBeWritten(t *testing.T) {
	cov43NeedChmod(t)
	dir, index := anIndexWithALandingBlock(t)
	hub := filepath.Join(dir, "hub.md")
	hubBefore := cov43Read(t, hub)
	idxBefore := cov43Read(t, index)
	if err := os.Chmod(index, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(index, 0o600) })

	var err error
	out := capture(t, func() { err = memFold([]string{"--hub", "hub"}) })
	if err == nil {
		t.Fatal("the fold reported success without writing the index")
	}
	if strings.Contains(out, "moved") {
		t.Errorf("claimed a move:\n%s", out)
	}
	if got := cov43Read(t, hub); got != hubBefore {
		t.Errorf("the hub was not put back:\n%s", got)
	}
	if got := cov43Read(t, index); got != idxBefore {
		t.Errorf("the index changed:\n%s", got)
	}
}

// A hub name with a directory in it passes the existence check but its link
// cannot be followed (the link syntax has no slash), so every folded memory
// would become unreachable. That is the post-condition's job to refuse.
func TestFoldRefusesAndRestoresWhenMemoriesWouldBecomeUnreachable(t *testing.T) {
	dir, index := anIndexWithALandingBlock(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMem(t, dir, "sub/hub.md", "A hub the index cannot point at.\n")
	hub := filepath.Join(dir, "sub", "hub.md")
	hubBefore := cov43Read(t, hub)
	idxBefore := cov43Read(t, index)

	var err error
	out := capture(t, func() { err = memFold([]string{"--hub", "sub/hub"}) })
	if err == nil || !strings.Contains(err.Error(), "refusing: 3 memories would stop being reachable") ||
		!strings.Contains(err.Error(), "one") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("got %v, want the unreachable refusal naming the memories", err)
	}
	if strings.Contains(out, "moved") {
		t.Errorf("claimed a move:\n%s", out)
	}
	if got := cov43Read(t, hub); got != hubBefore {
		t.Errorf("the hub was not put back:\n%s", got)
	}
	if got := cov43Read(t, index); got != idxBefore {
		t.Errorf("the index was not put back:\n%s", got)
	}
}
