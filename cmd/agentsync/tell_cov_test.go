package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cov43NeedChmod skips where a permission bit cannot make a write fail:
// Windows has no such bits and root ignores them.
func cov43NeedChmod(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop this user")
	}
}

// cov43FileForStore makes AGENTSYNC_DIR a regular file, so the lock directory
// would have to live under a file: every store operation fails for real.
func cov43FileForStore(t *testing.T, owner string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTSYNC_DIR", f)
	t.Setenv("AGENTSYNC_OWNER", owner)
}

func cov43CaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

func TestTellRefusesFileAndInlineTextTogether(t *testing.T) {
	sandbox(t, "sender-1111")
	p := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(p, []byte("from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := tell([]string{"-f", p, "o/r", "and inline"})
	if err == nil || !strings.Contains(err.Error(), "two ways to say the same thing") {
		t.Fatalf("got %v, want the two-ways refusal", err)
	}
	live, _, _ := store().Messages("o/r", 0)
	if len(live) != 0 {
		t.Errorf("%d message(s) stored despite the refusal", len(live))
	}
}

func TestTellNamesAnUnreadableMessageFile(t *testing.T) {
	sandbox(t, "sender-1111")
	missing := filepath.Join(t.TempDir(), "absent.txt")
	err := tell([]string{"-f", missing, "o/r"})
	if err == nil || !strings.Contains(err.Error(), "reading the message from "+missing) {
		t.Fatalf("got %v, want a refusal naming the file", err)
	}
}

func TestTellRefusesASessionIdAsAResource(t *testing.T) {
	sandbox(t, "sender-1111")
	err := tell([]string{"84a9d912", "please stop"})
	if err == nil || !strings.Contains(err.Error(), "is a session id, not a resource") {
		t.Fatalf("got %v, want the session-id refusal", err)
	}
	live, _, _ := store().Messages("84a9d912", 0)
	if len(live) != 0 {
		t.Errorf("%d message(s) left on a session id", len(live))
	}
}

func TestTellReportsAStoreItCannotWrite(t *testing.T) {
	cov43FileForStore(t, "sender-1111")
	var err error
	out := capture(t, func() { err = tell([]string{"o/r", "hello"}) })
	if err == nil {
		t.Fatal("tell succeeded against a store that cannot exist")
	}
	if strings.Contains(out, "stored") {
		t.Errorf("claimed to have stored something:\n%s", out)
	}
}

func TestParseAnywhereReturnsTheFlagError(t *testing.T) {
	for _, args := range [][]string{{"--nope"}, {"pos", "--nope"}} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		pos, err := parseAnywhere(fs, args)
		if err == nil || pos != nil {
			t.Errorf("%v: got (%v, %v), want (nil, error)", args, pos, err)
		}
	}
}

func TestMessagesReportsAStoreItCannotRead(t *testing.T) {
	cov43FileForStore(t, "sender-1111")
	if err := messages([]string{"o/r"}); err == nil {
		t.Fatal("messages succeeded against a store that cannot be read")
	}
}

func TestMessagesSaysHowManyItDropped(t *testing.T) {
	sandbox(t, "sender-1111")
	capture(t, func() {
		if err := tell([]string{"o/r", "kept"}); err != nil {
			t.Fatal(err)
		}
	})
	var out, errOut string
	errOut = cov43CaptureStderr(t, func() {
		out = capture(t, func() {
			if err := messages([]string{"o/r"}); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(out, "kept") || errOut != "" {
		t.Fatalf("nothing was dropped yet: stdout %q stderr %q", out, errOut)
	}

	files, _ := filepath.Glob(filepath.Join(root(), "locks", "*.messages"))
	if len(files) != 1 {
		t.Fatalf("%d message files, want 1", len(files))
	}
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	errOut = cov43CaptureStderr(t, func() {
		out = capture(t, func() {
			if err := messages([]string{"o/r"}); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(out, "kept") {
		t.Errorf("the good message vanished:\n%s", out)
	}
	if !strings.Contains(errOut, "1 older or unreadable message(s) not shown") {
		t.Errorf("did not say what it dropped: %q", errOut)
	}
}

func TestForgetNeedsExactlyOneResource(t *testing.T) {
	sandbox(t, "sender-1111")
	for _, args := range [][]string{{}, {"a", "b"}} {
		err := forget(args)
		if err == nil || !strings.Contains(err.Error(), "usage: agentsync forget") {
			t.Errorf("%v: got %v, want the usage refusal", args, err)
		}
	}
}

func TestForgetReportsAStoreItCannotRead(t *testing.T) {
	cov43FileForStore(t, "sender-1111")
	var err error
	out := capture(t, func() { err = forget([]string{"o/r"}) })
	if err == nil || strings.Contains(out, "forgot") {
		t.Fatalf("got err %v, out %q; want a failure and no claim of forgetting", err, out)
	}
}

func TestForgetReportsAMessageFileItCannotRemove(t *testing.T) {
	cov43NeedChmod(t)
	sandbox(t, "sender-1111")
	capture(t, func() {
		if err := tell([]string{"o/r", "stays"}); err != nil {
			t.Fatal(err)
		}
	})
	locks := filepath.Join(root(), "locks")
	if err := os.Chmod(locks, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locks, 0o700) })

	var err error
	out := capture(t, func() { err = forget([]string{"o/r"}) })
	if err == nil || strings.Contains(out, "forgot") {
		t.Fatalf("got err %v, out %q; want a failure and no claim of forgetting", err, out)
	}
	_ = os.Chmod(locks, 0o700)
	live, _, _ := store().Messages("o/r", 0)
	if len(live) != 1 {
		t.Errorf("%d message(s) left, want the 1 that could not be removed", len(live))
	}
}

func TestTellWarnsWhenTheMessageIsTheNameOfAFile(t *testing.T) {
	sandbox(t, "sender-1111")
	p := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut := cov43CaptureStderr(t, func() {
		capture(t, func() {
			if err := tell([]string{"o/r", p}); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(errOut, "did you mean `-f "+p+"`") {
		t.Errorf("no warning:\n%s", errOut)
	}
	live, _, _ := store().Messages("o/r", 0)
	if len(live) != 1 || live[0].Text != p {
		t.Errorf("stored %v, want the path as text", live)
	}
	// And a message that is not a path draws no warning.
	errOut = cov43CaptureStderr(t, func() {
		capture(t, func() { _ = tell([]string{"o/r", "plain words"}) })
	})
	if errOut != "" {
		t.Errorf("warned about plain words: %q", errOut)
	}
}

func TestTellSaysWhenWhoHoldsItCannotBeRead(t *testing.T) {
	cov43NeedChmod(t)
	sandbox(t, "sender-1111")
	locks := filepath.Join(root(), "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write and search, no read: the message lands, the listing fails.
	if err := os.Chmod(locks, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locks, 0o700) })

	var err error
	out := capture(t, func() { err = tell([]string{"o/r", "hello"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "stored   hello") || !strings.Contains(out, "who holds it could not be read") {
		t.Errorf("did not say the holder is unknown:\n%s", out)
	}
	if strings.Contains(out, "NOBODY") {
		t.Errorf("claimed nobody holds it when that is not known:\n%s", out)
	}
}

func TestTellWithNothingToSayIsAUsageError(t *testing.T) {
	sandbox(t, "sender-1111")
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"o/r", ""}, {"-f", empty, "o/r"}} {
		err := tell(args)
		if err == nil || err.Error() != tellUsage {
			t.Errorf("%q: got %v, want exactly the usage", args, err)
		}
	}
}

// Messages fails to read but the directory is writable: forget must stop at
// the read, not go on to remove what it could not look at.
func TestForgetDoesNotRemoveWhatItCouldNotRead(t *testing.T) {
	cov43NeedChmod(t)
	sandbox(t, "sender-1111")
	capture(t, func() {
		if err := tell([]string{"o/r", "unreadable"}); err != nil {
			t.Fatal(err)
		}
	})
	files, _ := filepath.Glob(filepath.Join(root(), "locks", "*.messages"))
	if len(files) != 1 {
		t.Fatalf("%d message files, want 1", len(files))
	}
	if err := os.Chmod(files[0], 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })

	var err error
	out := capture(t, func() { err = forget([]string{"o/r"}) })
	if err == nil || strings.Contains(out, "forgot") {
		t.Fatalf("got err %v, out %q; want a failure", err, out)
	}
	if _, serr := os.Stat(files[0]); serr != nil {
		t.Errorf("the message file was removed anyway: %v", serr)
	}
}
