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

	"github.com/go-agentsync/agentsync/lease"
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
	defer func() { _, _ = s.Release(resource, owner()) }()
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

// rowLinking returns the index row that links to name's memory file, or "" if
// none does. It is the reachability test: a memory is present when something
// points at it, whatever the pointing text happens to say today.
func rowLinking(index, name string) string {
	target := "(" + name + ".md)"
	for _, ln := range strings.Split(index, "\n") {
		if strings.Contains(ln, target) {
			return ln
		}
	}
	return ""
}

// memoryFile is the memory a record points at: a sibling of the index, named
// after the record.
func memoryFile(r record) string {
	return filepath.Join(filepath.Dir(r.Index), r.Name+".md")
}

// dropRows removes every index row linking to name's memory file, and reports
// whether it removed any. The counterpart of rowLinking, for the case where
// the memory is the thing that went away.
func dropRows(index, name string) (string, bool) {
	target := "(" + name + ".md)"
	lines := strings.Split(index, "\n")
	out := make([]string, 0, len(lines))
	cut := false
	for _, ln := range lines {
		if strings.Contains(ln, target) {
			cut = true
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n"), cut
}

// reachedVia reports whether the index still reaches a record's memory at all,
// however many hops it takes. The walk reads every memory beside the index, so
// it is cached per index: one session's records all name the same one, and
// re-walking 743 files for each of them turned a check into a chore.
//
// ⚠ The cache is the CALLER's, not a package variable: one mem-verify run is
// one snapshot of the index, but two runs in one process (the tests) must not
// share an answer taken before the index was rewritten.
func reachedVia(r record, cache map[string]map[string]bool) bool {
	got, ok := cache[r.Index]
	if !ok {
		dir := filepath.Dir(r.Index)
		all, err := memoriesIn(dir)
		if err != nil {
			// ⛔ A WALK THAT COULD NOT READ MUST NOT REPORT "NOT REACHED". That
			// answer is indistinguishable from a real loss and would have every
			// row re-inserted at once. Caching the empty result would repeat it
			// for every record, so this deliberately does not cache.
			return false
		}
		got = reachableFrom(r.Index, dir, all)
		cache[r.Index] = got
	}
	return got[r.Name+".md"]
}

func memVerify(args []string) error {
	reach := map[string]map[string]bool{}
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
	missing, changed, gone := 0, 0, 0
	goneAt := map[int]bool{}
	for i, r := range rs {
		b, err := os.ReadFile(r.Index)
		if err != nil {
			return err
		}

		if strings.Contains(string(b), r.Line) {
			fmt.Printf("ok       %s\n", r.Name)
			continue
		}
		// The recorded text is gone, but the entry may simply have been
		// rewritten -- a line that has become false is meant to be corrected.
		// What matters is whether the index still reaches the memory file, so
		// that is what is checked. Re-inserting the old text here would restore
		// a stale claim and leave two rows for one memory, which is worse than
		// the loss it thinks it is repairing.
		if row := rowLinking(string(b), r.Name); row != "" {
			// ⛔ A ROW POINTING AT A FILE THAT IS NOT THERE IS NOT REACHABILITY.
			// This is exact and cannot misfire: the row's target IS <name>.md,
			// and <name>.md does not exist. Anything else is left alone.
			if _, err := os.Stat(memoryFile(r)); errors.Is(err, os.ErrNotExist) {
				if err := withIndexLease(func() error {
					b, err := os.ReadFile(r.Index)
					if err != nil {
						return err
					}
					out, cut := dropRows(string(b), r.Name)
					if !cut {
						return nil
					}
					return os.WriteFile(r.Index, []byte(out), 0o644)
				}); err != nil {
					return err
				}
				fmt.Printf("gone     %s (dangling index row removed)\n", r.Name)
				goneAt[i] = true
				gone++
				continue
			}
			fmt.Printf("changed  %s\n", r.Name)
			rs[i].Line = row
			changed++
			continue
		}

		// ⛔⛔ UNREACHABLE IS NOT ALWAYS LOST, AND REPAIR MUST NOT FIGHT A
		// DELETION. Putting the row back for a memory whose file was removed on
		// purpose leaves the index pointing at nothing -- the same broken index
		// this command exists to prevent, only from the other side.
		//
		// Measured 2026-09-07: a handover note was deleted once its work was
		// done, its row taken out by hand, and the next mem-verify put the row
		// straight back.
		//
		// ⚠ AND THE CHECK BELONGS HERE, NOT BEFORE THE TWO ABOVE. Tried first
		// at the top of the loop, it declared three live entries deleted --
		// their record name is not their file name (mem-add was called with a
		// name of its own while the row links elsewhere), so <name>.md was
		// absent although the memory and its row were both fine. Nothing was
		// lost, because no row linked to those names either, but three entries
		// silently stopped being verified. Asking only once an entry is already
		// unreachable cannot make that mistake.
		if _, err := os.Stat(memoryFile(r)); errors.Is(err, os.ErrNotExist) {
			fmt.Printf("gone     %s (deleted, not put back)\n", r.Name)
			goneAt[i] = true
			gone++
			continue
		}

		// ⛔⛔ REACHABILITY IS TRANSITIVE, AND A MEMORY MOVED INTO A HUB HAS NOT
		// GONE MISSING. The index reaches it in two hops instead of one, which
		// is exactly what a hub file is for.
		//
		// Measured 2026-09-23: MEMORY.md had grown to 214 lines / 27 KB and the
		// reader truncated the last 19, hiding real memories at startup. The
		// only remedy is to fold rows into hub files -- and repairing on a
		// one-hop test put every folded row straight back, so the index could
		// never shrink. The command meant to protect the index was holding it
		// above its own read limit.
		if reachedVia(r, reach) {
			fmt.Printf("hubbed   %s\n", r.Name)
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
	if gone > 0 {
		// Forget the records whose memory is gone, so a later run neither
		// checks them nor puts their rows back.
		keep := make([]record, 0, len(rs)-gone)
		for i, r := range rs {
			if !goneAt[i] {
				keep = append(keep, r)
			}
		}
		rs = keep
	}
	if changed > 0 || gone > 0 {
		// Persist the rewritten text, so the next run says "ok" rather than
		// reporting the same edit for ever.
		if err := saveRecords(rs); err != nil {
			return err
		}
	}
	if changed > 0 {
		fmt.Printf("\n%d index entr%s been reworded since being added, and still reach the memory.\n", changed, map[bool]string{true: "y has", false: "ies have"}[changed == 1])
	}
	if gone > 0 {
		fmt.Printf("\n%d memor%s been deleted; nothing was put back for %s.\n",
			gone, map[bool]string{true: "y has", false: "ies have"}[gone == 1],
			map[bool]string{true: "it", false: "them"}[gone == 1])

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
	resources, rejected := sift(fs.Args())
	for _, r := range rejected {
		fmt.Fprintf(os.Stderr, "ignored  %q -- %s\n", r.arg, r.why)
	}
	if len(rejected) > 0 {
		fmt.Fprintln(os.Stderr, "         options go BEFORE the resources: agentsync claim --note \"...\" <resource>")
	}
	if len(resources) == 0 {
		return errors.New("nothing to claim: every argument was an option or a note")
	}
	s := store()
	failed := false
	for _, r := range resources {
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

// A rejection is an argument that is not a resource, and why.
type rejection struct{ arg, why string }

// sift separates the resources from what only reached the list because Go's
// flag package stops parsing at the first non-flag argument.
//
// "agentsync claim go-widgets/toolkit --note 'adding X'" therefore claims
// THREE things: the repository, the literal "--note", and the sentence. 25 of
// the 158 leases in the shared directory were like that -- 24 sentences and one
// "--note" -- written by two sessions that each believed they had said one
// thing.
//
// The repository itself does get claimed, so nothing was left unprotected. What
// it costs is the listing every session reads before deciding where to work,
// where a sixth of the entries were prose. So these are dropped and named
// rather than refused: the real resource beside them is still claimed, and
// "claim X && work" still proceeds.
// An option written without "=" takes the NEXT argument as its value, which is
// what the flag package would have done had it still been parsing. Dropping the
// option without its value would leave a one-word note -- "why", "urgent" --
// standing as a resource, indistinguishable from a repository by any test on
// the string itself.
func sift(args []string) (resources []string, rejected []rejection) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-"):
			rejected = append(rejected, rejection{a, "that is an option, not a resource"})
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				rejected = append(rejected, rejection{args[i], "that is " + a + "'s value"})
			}
		case strings.TrimSpace(a) == "":
			rejected = append(rejected, rejection{a, "that is empty"})
		case strings.ContainsAny(a, " \t"):
			rejected = append(rejected, rejection{a, "that reads as a note, not a resource"})
		default:
			resources = append(resources, a)
		}
	}
	return resources, rejected
}

func release(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentsync release <resource>...")
	}
	resources, rejected := sift(args)
	for _, r := range rejected {
		fmt.Fprintf(os.Stderr, "ignored  %q -- %s\n", r.arg, r.why)
	}
	s := store()
	for _, r := range resources {
		dropped, err := s.Release(r, owner())
		if err != nil {
			return err
		}
		if !dropped {
			fmt.Printf("not held  %s\n", r)
			continue
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
		"orphans": orphans,
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentsync {claim|release|claims|mem-add|mem-verify|orphans|whoami} ...")
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
