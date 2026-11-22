package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLinksInAnUnreadableFileIsNothing(t *testing.T) {
	if got := linksIn(filepath.Join(t.TempDir(), "absent.md")); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestMemoriesInNamesADirectoryItCannotRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	all, err := memoriesIn(dir)
	if err == nil || all != nil || !strings.Contains(err.Error(), "reading "+dir) {
		t.Fatalf("got (%v, %v), want a refusal naming %s", all, err, dir)
	}
}

func TestOrphansReportsADirectoryItCannotRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	var err error
	out := capture(t, func() { err = orphans(nil) })
	if err == nil || !strings.Contains(err.Error(), "reading "+dir) {
		t.Fatalf("got %v, want the read refusal", err)
	}
	if out != "" {
		t.Errorf("printed a report anyway:\n%s", out)
	}
}

func TestOrphansWithNoIndex(t *testing.T) {
	old := orphansIndex
	orphansIndex = func() string { return "" }
	t.Cleanup(func() { orphansIndex = old })
	var err error
	out := capture(t, func() { err = orphans(nil) })
	if out != "" {
		t.Errorf("printed a report anyway:\n%s", out)
	}
	if err == nil || !strings.Contains(err.Error(), "no memory index for this directory") {
		t.Fatalf("got %v, want the no-index refusal", err)
	}
}
