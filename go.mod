module github.com/go-agentsync/agentsync

// ⛔ The patch is what govulncheck judges the standard library by. At 1.27.1
// this module reported nine advisories whose fix is RELEASED in 1.27.2 --
// none of them called from here, but nobody could have known either way,
// because nothing ran the scan. The CI lane also reads this very line through
// `go-version-file: go.mod`, so the directive is the lane's toolchain too.
//
// It happened again, which is the useful part: on 2026-10-22 main went red on
// GO-2026-6809, fix RELEASED in 1.27.3, and stayed red for three days because
// a red default branch is nobody's PR. So this line is not a floor anybody
// chose for the language features it admits -- it is the toolchain the security
// gate judges, and it moves whenever a standard-library advisory is fixed.
// Raising it is the remedy vulngate names ("upgrade rather than carry it"), and
// the only one: an advisory with a released fix cannot be waived here.
go 1.27.3
