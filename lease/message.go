package lease

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"time"
)

// A Message is something one session needs the holder of a resource to know.
//
// It exists because the refusal was a dead end. `claim` names the holder and
// prints their note, which tells the refused session where to go -- and gives it
// nowhere to answer. On 2026-10-11 a session was refused go-crdt/collab by a
// holder whose note said it was about to measure and fix four reachable x/net
// advisories, work that had landed two days earlier and was already tagged. The
// refused session knew that. It had no way to say so, and the holder had no way
// to be told: agentsync coordinates by locks and nothing else, and the sessions
// it coordinates cannot see each other.
//
// A message is therefore not chat. It is the one thing a lock protocol is
// missing: a channel from whoever was turned away back to whoever is holding.
type Message struct {
	Resource string    `json:"resource"`
	From     string    `json:"from"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
}

// messagesPath is the lease's own path with a different suffix, so the resource
// is spelled the same way for both.
//
// That matters more than it looks: Normalize folds "https://github.com/o/r",
// "git@github.com:o/r" and "o/r" onto one name, and a message file that did its
// own sanitising would let two spellings of one repository leave notes in two
// places, each reader seeing half.
func (s Store) messagesPath(resource string) string {
	return strings.TrimSuffix(s.path(resource), ".lease") + ".messages"
}

// Tell leaves a message about a resource, for whoever holds it now or next.
//
// ONE WRITE, APPENDED. Not read-modify-write: that is exactly what dropped
// index lines out of a shared MEMORY.md when several sessions edited it at once,
// and this file has the same several writers. A single Write of a single line to
// a file opened O_APPEND is what the filesystem will not interleave, so two
// sessions telling at the same moment both arrive.
//
// A message with no text is refused rather than stored, because an empty note
// read by a holder is indistinguishable from no note at all and costs them the
// time to look.
func (s Store) Tell(resource, from, text string) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, errors.New("a message with nothing in it is worse than none: the holder reads it and learns nothing")
	}
	if strings.TrimSpace(resource) == "" {
		return Message{}, errors.New("a message needs a resource to be about")
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Message{}, err
	}

	m := Message{Resource: Normalize(resource), From: from, Text: text, At: s.now()}
	// Compact, and on one line: the reader splits by line, so a message
	// containing a newline would arrive as two half-messages. json.Marshal
	// escapes newlines inside the string, which is what keeps that true.
	body, err := json.Marshal(m)
	if err != nil {
		return Message{}, err
	}
	f, err := os.OpenFile(s.messagesPath(resource), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Message{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(body, '\n')); err != nil {
		return Message{}, err
	}
	// Reported, not assumed: a write that returned without error can still have
	// gone nowhere if the file is on something that lies, and this is the last
	// moment anybody could notice.
	return m, f.Sync()
}

// Messages reads what has been left about a resource, dropping anything older
// than maxAge and SAYING HOW MANY it dropped.
//
// The count is not decoration. A lease expires because a dead session must not
// block a resource for ever, and a message has the same problem the other way
// round: one left three days ago, about work since finished, is noise that
// teaches the next reader to skim. But silently discarding it would make a
// channel that quietly loses things, which is the one property a channel must
// not have. So they are dropped and counted, and the reader can ask for a
// longer window.
//
// A maxAge of zero means keep everything.
func (s Store) Messages(resource string, maxAge time.Duration) (live []Message, dropped int, err error) {
	f, err := os.Open(s.messagesPath(resource))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil // nothing was ever said, which is not a failure
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	now := s.now()
	sc := bufio.NewScanner(f)
	// A long message must not truncate into a different message: the default
	// 64KiB token limit would cut one and hand back a prefix that still parses.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// A line that does not parse is counted as dropped rather than
			// skipped in silence, for the same reason as a stale one.
			dropped++
			continue
		}
		if maxAge > 0 && now.Sub(m.At) > maxAge {
			dropped++
			continue
		}
		live = append(live, m)
	}
	if err := sc.Err(); err != nil {
		return nil, dropped, err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].At.Before(live[j].At) })
	return live, dropped, nil
}

// Forget removes every message about a resource, which is what a holder does
// once it has acted on them.
//
// Separate from reading on purpose. If reading cleared them, two holders in a
// row would see different halves of the same conversation, and a session that
// read them and then died would have consumed them for everybody.
func (s Store) Forget(resource string) error {
	err := os.Remove(s.messagesPath(resource))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
