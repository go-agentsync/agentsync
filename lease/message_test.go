package lease

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

var noon = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// Two sessions telling at the same moment both arrive.
//
// This is the property the whole file is shaped around. A shared MEMORY.md lost
// index lines because every writer read it, inserted, and wrote it all back;
// this file has the same several writers, so each message is ONE appended write
// and nothing is ever rewritten.
//
// Twenty goroutines is not a proof of atomicity -- nothing run a fixed number of
// times is -- but a read-modify-write implementation loses entries at this scale
// every time, which is what makes it a useful witness rather than a ritual.
func TestEveryTellArrivesWhenTheyRaceEachOther(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	const n = 20

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Tell("o/r", fmt.Sprintf("sess-%02d", i), fmt.Sprintf("message %02d", i)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	live, dropped, err := s.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped %d, want 0 — nothing here is stale or malformed", dropped)
	}
	if len(live) != n {
		t.Fatalf("read %d messages of %d: appending is not atomic the way this assumes", len(live), n)
	}
	seen := map[string]bool{}
	for _, m := range live {
		seen[m.Text] = true
	}
	for i := range n {
		if want := fmt.Sprintf("message %02d", i); !seen[want] {
			t.Errorf("%q never arrived", want)
		}
	}
}

// One spelling of a resource, however it was written.
//
// Normalize folds the forge URL, the SSH remote and the bare owner/repo onto one
// name. A message file that did its own sanitising would let two spellings leave
// notes in two places, each reader seeing half.
func TestEverySpellingOfAResourceReachesOneConversation(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	for _, spelling := range []string{
		"o/r",
		"https://github.com/o/r",
		"git@github.com:o/r",
	} {
		if _, err := s.Tell(spelling, "me", "about "+spelling); err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
	}
	// Read back through a fourth spelling, so the test is not simply asking the
	// same string it wrote.
	live, _, err := s.Messages("github.com/o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("read %d of 3 — the spellings are not reaching one file", len(live))
	}
	files, err := filepath.Glob(filepath.Join(s.Dir, "*.messages"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("three spellings made %d message files: %v", len(files), files)
	}
}

// Stale messages are dropped AND counted, never dropped quietly.
//
// A message left three days ago about work since finished is noise that teaches
// the next reader to skim. Discarding it in silence would make a channel that
// loses things, which is the one thing a channel must not do.
func TestStaleMessagesAreDroppedAndCounted(t *testing.T) {
	dir := t.TempDir()
	old := Store{Dir: dir, Now: at(noon.Add(-72 * time.Hour))}
	if _, err := old.Tell("o/r", "a", "three days ago"); err != nil {
		t.Fatal(err)
	}
	fresh := Store{Dir: dir, Now: at(noon)}
	if _, err := fresh.Tell("o/r", "b", "just now"); err != nil {
		t.Fatal(err)
	}

	// The control first: with no window, both are there — so what follows is
	// about the age and not about one of them failing to arrive.
	all, dropped, err := fresh.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || dropped != 0 {
		t.Fatalf("with no window: %d live, %d dropped, want 2 and 0", len(all), dropped)
	}

	live, dropped, err := fresh.Messages("o/r", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Text != "just now" {
		t.Errorf("live = %v, want only the recent one", live)
	}
	if dropped != 1 {
		t.Errorf("dropped %d, want 1 — a reader must be told what it is not seeing", dropped)
	}
}

// A line that does not parse is counted too, for the same reason.
func TestAMalformedLineIsCountedRatherThanSkipped(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	if _, err := s.Tell("o/r", "a", "good"); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.messagesPath("o/r"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	live, dropped, err := s.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Errorf("read %d good messages, want 1", len(live))
	}
	if dropped != 1 {
		t.Errorf("dropped %d, want 1", dropped)
	}
}

// A message that would arrive as two half-messages is stored as one.
//
// The reader splits by line, so a text containing a newline has to survive the
// round trip whole. json.Marshal escaping it is what makes that true, and this
// is the test that notices if the encoding is ever hand-rolled.
func TestAMessageWithNewlinesSurvivesWhole(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	text := "x/net v0.60.0 landed in #235\nand is tagged v0.77.0\n\t— nothing to redo"
	if _, err := s.Tell("o/r", "me", text); err != nil {
		t.Fatal(err)
	}
	live, dropped, err := s.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Fatalf("dropped %d: the newlines split it", dropped)
	}
	if len(live) != 1 {
		t.Fatalf("read %d messages, want 1: the newlines split it", len(live))
	}
	if live[0].Text != text {
		t.Errorf("text came back as %q", live[0].Text)
	}
}

// Nothing to say is refused rather than stored.
func TestAnEmptyMessageIsRefused(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	for _, text := range []string{"", "   ", "\n\t"} {
		if _, err := s.Tell("o/r", "me", text); err == nil {
			t.Errorf("a message of %q was accepted", text)
		}
	}
	if _, err := s.Tell("", "me", "something"); err == nil {
		t.Error("a message about nothing was accepted")
	}
	// And none of those created a file, so a reader is not sent to an empty one.
	if _, _, err := s.Messages("o/r", 0); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(s.Dir, "*.messages")); len(files) != 0 {
		t.Errorf("refused messages still wrote %v", files)
	}
}

// Reading does not consume: Forget is separate, and deliberately.
//
// If reading cleared them, two holders in a row would see different halves of
// one conversation, and a session that read them and then died would have
// consumed them for everybody.
func TestReadingDoesNotConsumeAndForgetDoes(t *testing.T) {
	s := Store{Dir: t.TempDir(), Now: at(noon)}
	if _, err := s.Tell("o/r", "a", "once"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		live, _, err := s.Messages("o/r", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 1 {
			t.Fatalf("read %d on pass %d: reading consumed it", len(live), i+1)
		}
	}
	if err := s.Forget("o/r"); err != nil {
		t.Fatal(err)
	}
	live, _, err := s.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("Forget left %d behind", len(live))
	}
	// Forgetting what was never said is not an error: a holder clearing its
	// inbox should not have to know whether it had one.
	if err := s.Forget("never/told"); err != nil {
		t.Errorf("Forget on nothing gave %v", err)
	}
}

// The ordering a reader gets is the order things were said.
func TestMessagesComeBackOldestFirst(t *testing.T) {
	dir := t.TempDir()
	for i, d := range []time.Duration{2 * time.Hour, 0, time.Hour} {
		s := Store{Dir: dir, Now: at(noon.Add(-d))}
		if _, err := s.Tell("o/r", "s", fmt.Sprintf("said %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	live, _, err := Store{Dir: dir, Now: at(noon)}.Messages("o/r", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range live {
		got = append(got, m.Text)
	}
	want := "said 0 said 2 said 1" // two hours ago, one hour ago, now
	if strings.Join(got, " ") != want {
		t.Errorf("order %v, want %q", got, want)
	}
}
