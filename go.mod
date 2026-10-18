module github.com/go-agentsync/agentsync

// ⛔ The patch is what govulncheck judges the standard library by. At 1.27.1
// this module reported nine advisories whose fix is RELEASED in 1.27.2 --
// none of them called from here, but nobody could have known either way,
// because nothing ran the scan. The CI lane also reads this very line through
// `go-version-file: go.mod`, so the directive is the lane's toolchain too.
go 1.27.2
