# Changelog

## v0.9.0

- `mem-add` refuses (exit 2) an index line that does not link `<memory-name>.md`,
  before writing anything. `<memory-name>` may carry `.md`. (#27)
- `mem-add <memory-name>` alone builds the line from the memory's frontmatter
  (`name`, `description`); a missing file or description is refused (exit 2).
- `mem-verify` no longer answers `ok` for text that names a memory without
  linking it: reachability is the `orphans` walk. A legacy record whose line has
  no link is reported `MISSING` and not re-inserted. (#27)
