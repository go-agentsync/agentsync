// Package modzip answers one question about this repository: can it be
// published as a Go module at all?
//
// ⛔ IT EXISTS BECAUSE v0.13.0 CANNOT BE INSTALLED. That tag added a fuzz seed
// named `lease/testdata/fuzz/FuzzNormalizePath/nul`, and the module zip format
// disallows `nul` as a path element because Windows reserves it as a device
// name. Every lane of CI was green, because every lane builds from a CHECKOUT:
// a working tree has no such rule, so nothing between the commit and the release
// ever packed the module. The first thing that did was a session running
//
//	go install github.com/go-agentsync/agentsync/cmd/agentsync@v0.13.0
//
// which failed with `create zip: malformed file path`. The tag stands -- a tag is
// never moved or reused -- so the repair is a later version, and this check is
// what keeps the next one from needing one.
//
// ⛔⛔ AND THE SUCCESS WAS WORSE THAN THE FAILURE. The same install written
// `@latest` exited **0** and left the PREVIOUS version in place: the proxy had
// not indexed the new tag yet, so "latest" was v0.12.1, and only reading
// `go version -m` on the binary showed that nothing had been installed. An exit
// status of 0 from `go install @latest` says a binary is there, not that it is
// the one asked for.
//
// The oracle is golang.org/x/mod/zip, the same package the module proxy uses, so
// this check cannot drift from the rule it is checking. Reimplementing the path
// rules here would be a second, slowly diverging copy of them.
package modzip

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

// Check reports whether dir can be packed as the module zip for path at version
// v, discarding the archive: the answer is the error, never the bytes.
//
// The version is only a label inside the archive, so any valid one answers the
// question; callers pass a fixed placeholder rather than the repository's real
// tag, which would make the check depend on where it is run from.
func Check(dir, path, v string) error {
	m := module.Version{Path: path, Version: v}
	if err := zip.CreateFromDir(io.Discard, m, dir); err != nil {
		return fmt.Errorf("this tree cannot be published as a Go module: %w", err)
	}
	return nil
}

// absPath and dirOf are vars so a test can reach the two answers no input can
// produce: `filepath.Abs` fails only for a process with no working directory,
// and a `filepath.Dir` that never shortens its argument is what the bound below
// exists for. Same reason `normalizeStep` is a var in lease and `statFile` is
// one in cmd/agentsync.
var (
	absPath = filepath.Abs
	dirOf   = filepath.Dir
)

// Root walks up from dir to the directory holding go.mod.
//
// A test cannot assume the module root is its own working directory: `go test`
// runs each package in its own, so the root is two levels up for one package and
// one for another, and hard-coding `..` ties the check to where its file lives.
//
// ⛔ The walk is BOUNDED, and not because a path is ever that deep. `mutsweep`
// forced `parent == abs` false and the mutation HUNG: that one comparison was
// the loop's only exit, exactly the defect this repository already fixed in
// lease.Normalize. The limit is taken ONCE -- re-measuring it each pass is how
// the first attempt at that fix outran its own bound -- and one pass per
// character is far more than any path needs, since every pass must shorten the
// string or be the last.
func Root(dir string) (string, error) {
	abs, err := absPath(dir)
	if err != nil {
		return "", fmt.Errorf("no absolute path for %s: %w", dir, err)
	}
	limit := len(abs) + 1
	for i := 0; i < limit; i++ {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}
		parent := dirOf(abs)
		if parent == abs {
			return "", fmt.Errorf("no go.mod at or above %s", dir)
		}
		abs = parent
	}
	return "", fmt.Errorf("walking up from %s did not reach a filesystem root in %d steps", dir, limit)
}
