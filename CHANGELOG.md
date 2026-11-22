# Changelog

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
