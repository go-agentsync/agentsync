# Changelog

## Unreleased

- ⛔ **v0.13.0 could not be installed, and nothing here could have noticed.**
  The seed `lease/testdata/fuzz/FuzzNormalizePath/nul`, named after the input it
  holds, is a path the Go module zip format refuses: `nul` is a reserved Windows
  device name. So `go install …/cmd/agentsync@v0.13.0` fails in every clone while
  **every CI lane stays green**, because all of them build from a *checkout* —
  and a working tree has no such rule. Nothing between the commit and the release
  ever packed the module; the first thing that did was a session installing the
  tag. The seed is renamed `nul-byte` (its bytes are untouched, so it is still
  replayed), and `internal/modzip` now packs the whole tree through
  `golang.org/x/mod/zip` — the same package the module proxy uses, so the check
  cannot drift from the rule — with a control that rebuilds the rejected tree in
  a `t.TempDir()` and requires the refusal. It runs in the ordinary `go test
  ./...` lane, so no new CI job and no release step can skip it. ⛔⛔ And the
  worse half: the same install written `@latest` **exited 0** and left v0.12.1 in
  place, because the proxy had not indexed the tag yet — only `go version -m` on
  the binary showed that nothing had been installed. Pin the version when
  verifying a release; `@latest` answers about the proxy, not about the tag.
- The new check arrived with the defect it was written to prevent elsewhere:
  `mutsweep` forced `parent == abs` false in `modzip.Root` and the mutation
  **HUNG**, because that one comparison was the walk's only exit — the same
  shape as `lease.Normalize` two entries above, written the same day. The walk
  is bounded by one pass per character of the path, taken **once**, and the two
  answers no input can produce (`filepath.Abs` failing, a `filepath.Dir` that
  never shortens) are reached through vars a test replaces, as `normalizeStep`
  and `statFile` already are. So `internal/modzip` joins the coverage gate's
  `-coverpkg` rather than being excused from it: 1085 blocks, 0 uncovered
  (1073 before this branch).
- The headings below were corrected. v0.12.1 (`ab842395`) and v0.13.0
  (`a545862a`) had both been cut while their entries sat under "Unreleased", and
  the rebase that carried the fuzz work onto the released main left its three
  entries *inside* the v0.12.0 section — so the file attributed them to a tag
  published before they were written. Each tag now owns its own entries, checkable
  against `git rev-list -n1 <tag>`.

## v0.13.0

⛔ **This tag cannot be installed.** `go install
github.com/go-agentsync/agentsync/cmd/agentsync@v0.13.0` fails with `create zip:
malformed file path "lease/testdata/fuzz/FuzzNormalizePath/nul": "nul"
disallowed as path element component on Windows`. The seed file below is named
after the input it holds, a NUL byte, and `nul` is a reserved device name that
the module zip format refuses. Use **v0.13.1**, which carries the same changes
under a renamed seed plus the check that would have caught this. The tag is left
where it is: a tag is never moved or reused.

- **Fuzz targets and a committed seed corpus for every parser of input this
  program does not write.** Nine targets (`FuzzParseLease`, `FuzzParseTells`,
  `FuzzParseTakes`, `FuzzNormalizePath`, `FuzzParseRecords`, `FuzzGateHook`,
  `FuzzGateHookPayload`, `FuzzGatePost`, `FuzzGatePostPayload`), seeds under
  `testdata/fuzz/<Target>/` in the layout `go-agentsync/agentscope` uses, and a
  `fuzz` CI lane of 20 s per target. The gate targets assert what the gate is
  for (a publish into an unheld repository is denied; only a finishing command
  releases a lease), not that it survives. `FuzzGatePost` asserts the strict
  property — no release without evidence of success — unconditionally. It was
  written behind a `postsuccess` build tag because it was red on five seeds (a
  refused `403`, a non-zero exit, an interrupted run, an empty result, a null
  result), each of which released a lease; the tag is gone now that v0.12.0
  carries the `notSucceeded` fix those seeds measured, so the five are a
  permanent assertion rather than a record of a defect.
- **`Normalize` is a fixed point.** `FuzzNormalizePath` found, on its first run,
  that `githuB.Com/ 0` normalizes to `" 0"` — a leading space, quoted because a
  code span swallows one — and a second pass to `"0"`: the gate
  normalizes a repository and the store normalizes it again, so one name could
  reach two keys. It now repeats until the key stops moving — and the repetition
  is **bounded**, which the fixed-point change was not. `mutsweep` negated the
  loop's `next == s` and the mutation HUNG instead of failing a test: the only
  thing between `Normalize` and an endless loop was that comparison plus a
  comment arguing about a different function. Both gates run on every Bash
  command this machine issues and normalize names read from other sessions'
  lease files, so a step that ever grew or cycled a string would hang the
  command rather than mis-key a lease. The limit is `len(s)+1` passes, taken
  **once**: written `i <= len(s)` it is re-measured every pass and a growing
  step outruns it — measured, still looping after 226,430 passes. Two tests hold
  it: a divergent step must return, and a name that settles must cost exactly two
  passes (the mutant returns the same string, so only the work tells them apart).
- **An empty `tool_response` released a lease**, and the corpus above is how it
  was found. `notSucceeded` (v0.12.0) fails closed on a refused `403`, a non-zero
  exit, an interruption, a missing response and a null one — five of the six
  shapes `FuzzGatePost` seeds — but `"tool_response":{}` still read as a success.
  The v0.12.0 entry recorded that as accepted ("a failure delivered as a
  success-shaped object with none of those fields still releases"); it is a
  defect, because an object with no fields is the same absence as no object, and
  every completed Bash call carries at least one of `stdout`, `stderr`,
  `interrupted` or `isImage`. The check counts fields rather than requiring
  `stdout` by name, so a harness that renames one keeps releasing leases instead
  of holding every one of them forever.

## v0.12.1

- Dependencies taken to the versions that exist: `gitsafe v0.58.1 → v0.59.0`
  (released 2026-11-22 15:01) and the CI lane's `vulngate v0.152.0 → v0.156.0`
  (16:01). `#24` had just brought them to v0.58.1 and v0.152.0 after sitting
  open for 17 days, and both were already behind on the day it merged — in the
  repository whose own job is judging, reading `gitsafe` to do it. ⛔ Measured,
  not assumed: the two `vulngate` binaries are **not** byte-identical, so unlike
  the 2026-10-18 bump recorded in `ci.yml` this one can change what the scan
  reports, and the comment there now says so. `v0.157.0` was published while
  this branch was open and is deliberately **not** taken: the claim here is a
  measurement of v0.152.0 against v0.156.0, and extending it by analogy to a
  version nobody hashed is the mistake this entry exists to record.

## v0.12.0

- **Every block of `cmd/agentsync` and `lease` is entered by a test, and CI
  refuses a change that leaves one out** (#43). Measured before: 80.1% of the
  command's statements, 84.6% of both packages, 174 blocks no test entered.
  After: 0. The gate counts blocks (`covblocks --max 0`), not a percentage,
  because `go tool cover -func` rounds and a gate written against it passes at
  99.95%.
- **A bad flag is an exit status the tests can reach.** Every command parsed its
  flags with `flag.ExitOnError`, which calls `os.Exit` inside the command, so no
  test could ever see it. `newFlags`/`parseFlags` return an `exitError` instead
  (2 for a bad flag, 0 for `-h`: what `ExitOnError` did) and `run()` turns it
  into the status, leaving `main()` one line. No behaviour changes.
- `gate --hook` refuses `agentsync claim --wait` when the Bash call has
  `run_in_background: true`: the wait outlives its turn and the lease is held by
  nobody (lesson recurred 2026-11-22).
- `gate --post` releases only when the payload shows a zero exit (fail closed):
  PostToolUseFailure, `error`, `is_error`, a missing or string `tool_response`,
  `interrupted`, a command run with `run_in_background` (its status is not
  known when it detaches) and a non-zero `exit_code`/`exitCode` release nothing. The
  payload documents no exit status, so a failure delivered as a success-shaped
  object with none of those fields still releases.
- **A test can no longer reach the shared claims directory.** `root()` panics
  under `go test` when `AGENTSYNC_DIR` is unset, instead of returning the path
  where every live session's leases are. It found two writers on its first run:
  the fold tests, which sandboxed the memory index and then took real leases
  through `withIndexLease`, and — in the shared store itself — `o/r` held by
  `11111111`, a test's fixture owner, written by a clone older than 2db6442.
  `go build` keeps the default path; the binary grows 16,848 bytes (+0.34%).
- **`.`, `..` and a path are no longer claimed as resources.** `claim .` took a
  lease on the literal string `.`, which every session's working directory maps
  onto: one global lock that two sessions in unrelated checkouts both hold while
  believing they have coordinated, and that tells a third nothing. Measured
  2026-11-22, both live the same day: `.` held until 20:02, and `..` written at
  14:54. Dropped and named like the other non-resources, so the repository
  beside the mistake is still claimed, and the reason says to name the
  repository as `git remote -v` spells it. A name without a slash stays valid —
  worktree topics are claimed that way here.
- `tell` refuses a resource that is a **session id** — `short()`'s eight hex
  characters, or a raw UUID — because a message left on one opens a queue nobody
  will ever claim. The refusal names what that session holds, so the message has
  somewhere to go. Measured 2026-11-22: twice in one hour, by one session, and
  both times the output said in full "NOBODY holds it right now". A true line is
  not a gate. A resource the store has already seen is never refused, so a
  repository whose name happens to be hex keeps working.
- `gate --hook`'s tracker notice told every session to run the command the point
  above now refuses: its `agentsync tell` argument was `short(l.Owner)`, a
  **session id**, where `tell` takes a resource. It names the repository now.
  This is the cause, not a second symptom — the session that lost two handovers
  was doing exactly what the notice said. A test takes the command out of the
  notice and hands it to `tell`, so the two sides cannot drift apart again.

## v0.11.1

- `gate --hook` no longer refuses a command that publishes nothing: `-h`,
  `--help`, a leading `help`, `--version` and `--dry-run` need no claim at all.
  Measured 2026-11-22, `ghrelease --help` was refused in a clone of a repository
  nobody held — and reading a program's flags is what one does before deciding
  whether to claim anything. (#30)
- The predicate both gates now share is stricter than the one `gate --post`
  shipped with, because calling it from the pre-tool gate removed its safe
  direction: a bare `help` counts only as the first argument (`--title help` is a
  value), and `--dry-run=false` is a publish rather than a dry run. An
  unparseable value counts as publishing.
- `gate --post` no longer releases a lease for an invocation that cannot have
  published anything: `-h`, `--help`, `help` and `--version` join `--dry-run`.
  Measured 2026-11-22, a plain `ghrelease -h` released a lease on a repository
  whose work was unfinished; help exits 0 and writes no release, so the exit
  status could never have told them apart.
- The claim that Claude Code runs PostToolUse only for a command that succeeded
  is removed from the code comment and the README, because it is false: a `Bash`
  call running a program that exits non-zero is a tool call that worked. A
  `ghmerge` refused with a 403 released the lease on a merge that never happened.

## v0.11.0

- A succeeded `ghmerge`/`ghrelease` releases the holder's lease on that repository
  (`agentsync gate --post`, a PostToolUse hook), unless it was claimed with
  `--keep-lease "<why>"`; the reason is stored and shown in `claims`. (#33)
- A lease idle for 15 minutes (`AGENTSYNC_IDLE`, `0` disables) may be taken by
  another session; `claims` shows `idle N`, the take is logged in `takes.log` and
  the previous holder is left a message. A gated command the holder is allowed to
  run counts as activity. (#33)
- `claim --wait` is dropped when its session's transcript has been silent for
  12 minutes (`AGENTSYNC_WAIT_GRACE`). (#33)
- Take, renew, touch and release of one lease are serialised by a per-lease lock,
  so a holder renewing cannot lose its lease to a taker reading the old one.

## v0.9.0

- `mem-add` refuses (exit 2) an index line that does not link `<memory-name>.md`,
  before writing anything. `<memory-name>` may carry `.md`. (#27)
- `mem-add <memory-name>` alone builds the line from the memory's frontmatter
  (`name`, `description`); a missing file or description is refused (exit 2).
- `mem-verify` no longer answers `ok` for text that names a memory without
  linking it: reachability is the `orphans` walk. A legacy record whose line has
  no link is reported `MISSING` and not re-inserted. (#27)
