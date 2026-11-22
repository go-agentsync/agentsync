package lease

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Fuzz targets for every parser of a file other sessions write. The seed corpus
// is committed under testdata/fuzz/<Target>/, in the layout go-agentsync/agentscope
// uses (collect/testdata/fuzz/FuzzParseLease, FuzzParseTells, ...), so the two
// repositories read alike. `go test` replays every seed on every run; CI runs
// each target for a bounded time on top (see .github/workflows/ci.yml).
//
// Every target works under t.TempDir(): the shared claims directory is never
// opened, and none of these functions reads the environment.

// FuzzParseLease feeds one lease file. About 25 sessions write these
// concurrently, so a torn or partial file is a realistic input.
//
// Properties, beyond "does not panic":
//   - a lease that parses always names a resource and an owner (the gate decides
//     on those two fields; an ownerless lease must read as torn, not as held);
//   - a lease that parses survives a write and a read back unchanged;
//   - a directory holding the file is either listed or reported unreadable by
//     Covering, never silently treated as empty over a file nobody could read.
func FuzzParseLease(f *testing.F) {
	for _, s := range []string{
		`{"resource":"o/r","owner":"a","pid":1,"acquired":"2026-11-22T20:00:00Z","expires":"2026-11-22T20:45:00Z"}`,
		`{"resource":"o/r","owner":"a","keep":"waiting on CI"}`,
		`{"resource":"o/r"}`,
		`{"resource":"o/r","owner":"a","pid":1,"acq`,
		``, `null`, `[]`, `{}`, `{"resource":1,"owner":2}`,
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	s := Store{Dir: dir}
	p := filepath.Join(dir, "x.lease")
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := s.read(p)
		_, cerr := s.Covering("o/r")
		if err != nil {
			if cerr == nil {
				t.Fatalf("Covering answered over a file read() refused: %q", data)
			}
			return
		}
		if l.Resource == "" || l.Owner == "" {
			t.Fatalf("an incomplete lease was accepted: %+v from %q", l, data)
		}
		q := filepath.Join(dir, "y.lease")
		defer os.Remove(q)
		if _, err := s.write(q, l); err != nil {
			t.Fatal(err)
		}
		back, err := s.read(q)
		if err != nil {
			t.Fatalf("a lease that parsed could not be read back: %v", err)
		}
		if back.Resource != l.Resource || back.Owner != l.Owner || back.PID != l.PID ||
			back.Note != l.Note || back.Keep != l.Keep ||
			!back.Acquired.Equal(l.Acquired) || !back.Expires.Equal(l.Expires) || !back.Active.Equal(l.Active) {
			t.Fatalf("round trip changed the lease:\n%+v\n%+v", l, back)
		}
	})
}

// FuzzParseTells feeds the message log, one JSON object per line.
//
// Property: every non-blank line is accounted for, as live or as dropped. The
// doc comment on Messages promises a channel that never loses a message
// silently, and that is a count, so it is asserted as one. The live ones come
// back oldest first.
func FuzzParseTells(f *testing.F) {
	for _, s := range []string{
		`{"resource":"o/r","from":"a","text":"hi","at":"2026-11-22T20:00:00Z"}` + "\n",
		`{"resource":"o/r","from":"a","text":"two\nlines","at":"2026-11-22T20:00:00Z"}` + "\n" + `{"resource":"o/r","from":"b","text":"x","at":"2026-11-22T19:00:00Z"}` + "\n",
		"\n\n  \n", "not json\n", `{"resource":"o/r"`, "{}\r\n", "",
	} {
		f.Add([]byte(s), int64(0))
	}
	dir := f.TempDir()
	now := time.Date(2026, 11, 22, 20, 30, 0, 0, time.UTC)
	s := Store{Dir: dir, Now: func() time.Time { return now }}
	f.Fuzz(func(t *testing.T, data []byte, maxAgeSec int64) {
		if err := os.WriteFile(s.messagesPath("o/r"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		// A bounded window: a Duration near its limits makes now.Sub(m.At)
		// overflow in the test's arithmetic, not in the parser.
		maxAge := time.Duration(maxAgeSec%86400) * time.Second
		if maxAge < 0 {
			maxAge = -maxAge
		}
		live, dropped, err := s.Messages("o/r", maxAge)
		if err != nil {
			return // a line over the 1 MiB cap is reported, which is the contract
		}
		lines := 0
		for _, ln := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(ln) != "" {
				lines++
			}
		}
		if len(live)+dropped != lines {
			t.Fatalf("%d live + %d dropped != %d non-blank lines in %q", len(live), dropped, lines, data)
		}
		if !sort.SliceIsSorted(live, func(i, j int) bool { return live[i].At.Before(live[j].At) }) {
			t.Fatalf("live messages are not oldest first: %+v", live)
		}
	})
}

// FuzzParseTakes feeds the take-over log (takes.log) and the line splitter under
// it.
//
// Properties: splitLines neither loses nor invents a byte (what it returns,
// joined, is the input without its newlines) and never returns an empty line;
// Takes never returns more records than the input has lines.
func FuzzParseTakes(f *testing.F) {
	for _, s := range []string{
		`{"resource":"o/r","from":"a","to":"b","at":"2026-11-22T20:00:00Z","idle_for":"31m0s"}` + "\n",
		"a\n\nb", "\n", "", `{"resource":`, "{}\n{}\n",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	s := Store{Dir: dir}
	f.Fuzz(func(t *testing.T, data []byte) {
		lines := splitLines(data)
		var joined []byte
		for _, ln := range lines {
			if len(ln) == 0 {
				t.Fatalf("splitLines returned an empty line for %q", data)
			}
			joined = append(joined, ln...)
		}
		if want := bytes.ReplaceAll(data, []byte("\n"), nil); !bytes.Equal(joined, want) {
			t.Fatalf("splitLines changed the bytes: %q -> %q", data, joined)
		}
		if err := os.WriteFile(s.takesPath(), data, 0o644); err != nil {
			t.Fatal(err)
		}
		ts, err := s.Takes()
		if err != nil {
			t.Fatal(err)
		}
		if len(ts) > len(lines) {
			t.Fatalf("%d takes from %d lines", len(ts), len(lines))
		}
	})
}

// FuzzNormalizePath feeds a resource name, which arrives from a command line and
// from a lease file. Normalize must be idempotent (two sessions landing on the
// same key is the whole point of it), and path must stay inside the store: a
// resource is untrusted text and names a file.
func FuzzNormalizePath(f *testing.F) {
	for _, s := range []string{
		"o/r", "github.com/O/R.git", "git@github.com:o/r.git", "https://github.com/o/r/pull/3",
		"../../etc/passwd", "/abs/path", "a/../../b", "..", ".", "", "\x00", strings.Repeat("a/", 200),
	} {
		f.Add(s)
	}
	s := Store{Dir: f.TempDir()}
	f.Fuzz(func(t *testing.T, res string) {
		n := Normalize(res)
		if Normalize(n) != n {
			t.Fatalf("Normalize is not idempotent: %q -> %q -> %q", res, n, Normalize(n))
		}
		p := s.path(res)
		if filepath.Dir(p) != s.Dir {
			t.Fatalf("resource %q names a file outside the store: %s", res, p)
		}
		if !strings.HasSuffix(p, ".lease") || strings.ContainsAny(filepath.Base(p), "/\\\x00") {
			t.Fatalf("resource %q makes a path %q that is not one flat .lease file", res, p)
		}
	})
}

// The crasher FuzzNormalizePath found, kept by name so the reason survives the
// corpus file: one pass left " 0", a second made it "0".
func TestNormalizeIsAFixedPoint(t *testing.T) {
	for _, in := range []string{"githuB.Com/ 0", "http://https://o/r", "https://github.com/github.com/o/r.git.git"} {
		n := Normalize(in)
		if Normalize(n) != n {
			t.Errorf("Normalize(%q) = %q, which a second pass moves to %q", in, n, Normalize(n))
		}
	}
}
