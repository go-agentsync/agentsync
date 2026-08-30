// agentsync coordinates the Claude sessions that share this machine.
//
// It exists because 22 sessions were found working the same project at once,
// and nothing told any of them. Two failures followed, and both were silent:
//
//   - MEMORY.md is read-modify-written by every session. Index lines vanished
//     three times in one evening -- always the same two, because they sat in a
//     block some other writer re-emitted from a stale copy. Nothing broke: the
//     memory files stayed on disk, no link dangled, the file stayed valid. The
//     entries simply became unreachable, which is indistinguishable from never
//     having written them.
//   - A build campaign on go-pkgx/packages was interrupted by a manual dispatch
//     from a session with no way to know it was running.
//
// So: `claim` says out loud what you are working on, and `mem-add` makes the
// index edit atomic AND checks afterwards that it survived. The checking is the
// point. A write that reports success and is gone ten minutes later is the
// failure mode here, so `mem-verify` re-reads and repairs.
//
// It is advisory. It coordinates sessions that ask; it cannot stop one that
// does not -- which is why mem-verify repairs rather than trusts.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentsync/lease"
)

func home() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return os.Getenv("HOME")
}

func root() string {
	if v := os.Getenv("AGENTSYNC_DIR"); v != "" {
		return v
	}
	return filepath.Join(home(), ".claude", "agentsync")
}

// owner identifies this session as durably as it can. The session id is
// preferred because it survives the whole conversation and differs between
// sessions on one machine; the fallbacks only keep the tool usable elsewhere.
func owner() string {
	// CLAUDE_CODE_SESSION_ID is the session's UUID and is present in the
	// environment of every command it runs. The PID is deliberately NOT part of
	// this: each command is a new process, and an owner that changes between
	// two calls can neither extend nor release its own lease.
	for _, k := range []string{"AGENTSYNC_OWNER", "CLAUDE_CODE_SESSION_ID"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	h, _ := os.Hostname()
	return "unidentified-" + h
}

// memoryIndex locates MEMORY.md the way Claude does: the project directory's
// path with the separators flattened.
func memoryIndex() string {
	if v := os.Getenv("AGENTSYNC_MEMORY"); v != "" {
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(home(), ".claude", "projects", projectSlug(dir), "memory", "MEMORY.md")
}

// projectSlug is how Claude names a project's directory: the absolute path with
// both separators and underscores flattened to dashes. Underscores matter --
// /Users/david_delavennat becomes -Users-david-delavennat, and getting that
// wrong points the tool at a directory that does not exist.
func projectSlug(dir string) string {
	return strings.NewReplacer("/", "-", "_", "-").Replace(dir)
}

// short trims a session UUID for display. The full value stays in the lease.
func short(o string) string {
	if len(o) == 36 && strings.Count(o, "-") == 4 {
		return o[:8]
	}
	return o
}

func store() lease.Store { return lease.Store{Dir: filepath.Join(root(), "locks")} }

// record is what this session claims to have put in the index, so mem-verify
// can check it later without being told again.
type record struct {
	Name    string    `json:"name"`
	Line    string    `json:"line"`
	Section string    `json:"section,omitempty"`
	Index   string    `json:"index"`
	Added   time.Time `json:"added"`
}

func recordPath() string {
	return filepath.Join(root(), "sessions", owner()+".json")
}

func loadRecords() ([]record, error) {
	b, err := os.ReadFile(recordPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []record
	return rs, json.Unmarshal(b, &rs)
}

func saveRecords(rs []record) error {
	if err := os.MkdirAll(filepath.Dir(recordPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, recordPath())
}

// insert puts line into the index, once. It returns false when the line is
// already there, so calling twice is harmless -- a repair must never duplicate.
func insert(body, line, section string) (string, bool) {
	if strings.Contains(body, line) {
		return body, false
	}
	lines := strings.Split(body, "\n")
	if section != "" {
		for i, l := range lines {
			if strings.TrimSpace(l) != strings.TrimSpace(section) {
				continue
			}
			// Append at the END of that section: the last position before the
			// next heading. Inserting at the top puts it inside whatever block
			// a neighbouring writer is most likely to re-emit.
			j := i + 1
			last := i
			for ; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "#") {
					break
				}
				if strings.TrimSpace(lines[j]) != "" {
					last = j
				}
			}
			out := append([]string{}, lines[:last+1]...)
			out = append(out, line)
			out = append(out, lines[last+1:]...)
			return strings.Join(out, "\n"), true
		}
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + line + "\n", true
}

// enclosingSection reports the heading a line currently sits under. It is what
// makes a repair put the entry BACK where it was: registering a line that is
// already in the index without naming its section made the repair append at the
// end of the file, and a few of those would scramble the index's organisation.
func enclosingSection(body, line string) string {
	section := ""
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "#") {
			section = l
			continue
		}
		if strings.TrimSpace(l) != "" && strings.Contains(l, line) {
			return section
		}
	}
	return ""
}

func withIndexLease(fn func() error) error {
	s := store()
	const resource = "MEMORY.md"
	if _, err := s.Acquire(resource, owner(), "index edit", 2*time.Minute); err != nil {
		return fmt.Errorf("MEMORY.md is being edited: %w", err)
	}
	defer func() { _ = s.Release(resource, owner()) }()
	return fn()
}

func memAdd(args []string) error {
	fs := flag.NewFlagSet("mem-add", flag.ExitOnError)
	section := fs.String("section", "", "append at the end of this heading's section")
	idx := fs.String("index", memoryIndex(), "path to MEMORY.md")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: agentsync mem-add [--section H] <memory-name> <index-line>")
	}
	name, line := fs.Arg(0), fs.Arg(1)

	err := withIndexLease(func() error {
		b, err := os.ReadFile(*idx)
		if err != nil {
			return err
		}
		out, changed := insert(string(b), line, *section)
		if changed {
			if err := os.WriteFile(*idx, []byte(out), 0o644); err != nil {
				return err
			}
		}
		// Read it BACK. The whole reason this tool exists is that a write can
		// report success and be gone; trusting the write would reproduce the bug.
		after, err := os.ReadFile(*idx)
		if err != nil {
			return err
		}
		if !strings.Contains(string(after), line) {
			return errors.New("the line was not there when re-read; another writer overwrote it")
		}
		fmt.Printf("indexed %s%s\n", name, map[bool]string{true: "", false: " (already present)"}[changed])
		return nil
	})
	if err != nil {
		return err
	}

	rs, err := loadRecords()
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Name == name {
			return nil
		}
	}
	// Remember where the line actually lives, so a later repair restores it
	// there rather than at the bottom of the file.
	sec := *section
	if sec == "" {
		if b, err := os.ReadFile(*idx); err == nil {
			sec = enclosingSection(string(b), line)
		}
	}
	return saveRecords(append(rs, record{Name: name, Line: line, Section: sec, Index: *idx, Added: time.Now()}))
}

func memVerify(args []string) error {
	fs := flag.NewFlagSet("mem-verify", flag.ExitOnError)
	repair := fs.Bool("repair", true, "re-insert entries that have gone missing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rs, err := loadRecords()
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Println("nothing recorded for this session")
		return nil
	}
	missing := 0
	for _, r := range rs {
		b, err := os.ReadFile(r.Index)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), r.Line) {
			fmt.Printf("ok       %s\n", r.Name)
			continue
		}
		missing++
		if !*repair {
			fmt.Printf("MISSING  %s\n", r.Name)
			continue
		}
		if err := withIndexLease(func() error {
			b, err := os.ReadFile(r.Index)
			if err != nil {
				return err
			}
			out, _ := insert(string(b), r.Line, r.Section)
			return os.WriteFile(r.Index, []byte(out), 0o644)
		}); err != nil {
			return err
		}
		fmt.Printf("REPAIRED %s\n", r.Name)
	}
	if missing > 0 {
		fmt.Printf("\n%d entr%s had been dropped from the index.\n", missing, map[bool]string{true: "y", false: "ies"}[missing == 1])
	}
	return nil
}

func claim(args []string) error {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	ttl := fs.Duration("ttl", 45*time.Minute, "how long before anyone may take it")
	note := fs.String("note", "", "what you are doing, for the next session to read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: agentsync claim [--ttl d] [--note s] <resource>...")
	}
	s := store()
	failed := false
	for _, r := range fs.Args() {
		l, err := s.Acquire(r, owner(), *note, *ttl)
		var held *lease.Held
		switch {
		case errors.As(err, &held):
			failed = true
			fmt.Printf("REFUSED  %s\n         held by %s until %s\n", r, short(held.By.Owner), held.By.Expires.Format("15:04:05"))
			if held.By.Note != "" {
				fmt.Printf("         %q\n", held.By.Note)
			}
		case err != nil:
			return err
		default:
			fmt.Printf("claimed  %s until %s\n", r, l.Expires.Format("15:04:05"))
		}
	}
	if failed {
		return errUnavailable
	}
	return nil
}

var errUnavailable = errors.New("at least one resource is held by another session")

func release(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentsync release <resource>...")
	}
	s := store()
	for _, r := range args {
		if err := s.Release(r, owner()); err != nil {
			return err
		}
		fmt.Printf("released %s\n", r)
	}
	return nil
}

func claims([]string) error {
	ls, err := store().List()
	if err != nil {
		return err
	}
	if len(ls) == 0 {
		fmt.Println("no resource is claimed")
		return nil
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Resource < ls[j].Resource })
	now := time.Now()
	me := owner()
	for _, l := range ls {
		state := "held"
		if l.Expired(now) {
			state = "EXPIRED"
		}
		mine := ""
		if l.Owner == me {
			mine = "  (this session)"
		}
		fmt.Printf("%-8s %-42s %s until %s%s\n", state, l.Resource, short(l.Owner), l.Expires.Format("15:04:05"), mine)
		if l.Note != "" {
			fmt.Printf("         %q\n", l.Note)
		}
	}
	return nil
}

func whoami([]string) error {
	fmt.Printf("owner   %s\nlocks   %s\nindex   %s\n", owner(), filepath.Join(root(), "locks"), memoryIndex())
	return nil
}

func main() {
	cmds := map[string]func([]string) error{
		"claim": claim, "release": release, "claims": claims,
		"mem-add": memAdd, "mem-verify": memVerify, "whoami": whoami,
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentsync {claim|release|claims|mem-add|mem-verify|whoami} ...")
		os.Exit(2)
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "agentsync: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err := fn(os.Args[2:]); err != nil {
		if errors.Is(err, errUnavailable) {
			os.Exit(1) // a refusal is a result, not a crash
		}
		fmt.Fprintln(os.Stderr, "agentsync:", err)
		os.Exit(1)
	}
}
