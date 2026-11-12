package main

import (
	"testing"
	"time"
)

// Substitutions run inside double quotes, and a # inside a multi-line quoted
// string is text: both used to hide a publish.
func TestAPublishBehindQuotesIsFound(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`echo "$(ghmerge go-numerics/optimize 62)"`,
		"echo \"`ghmerge go-numerics/optimize 62`\"",
		`echo "x $(echo $(ghmerge go-numerics/optimize 62))"`,
		"git commit -m \"a\nb #x\" && ghmerge go-numerics/optimize 62",
		"git commit -m 'a\nb #x' && ghmerge go-numerics/optimize 62",
		"echo hi # note\nghmerge go-numerics/optimize 62",
		`nice -n 5 ghmerge go-numerics/optimize 62`,
		`sudo -u bob ghmerge go-numerics/optimize 62`,
		`env FOO=1 ~/.local/bin/ghmerge go-numerics/optimize 62`,
		`timeout 30 ghmerge go-numerics/optimize 62`,
		`echo 62 | xargs -I{} ghmerge go-numerics/optimize {}`,
		`eval ghmerge go-numerics/optimize 62`,
		`~/.local/bin/ghmerge go-numerics/optimize 62`,
	} {
		if denied, _, _ := ask(env, payload(t, me, "/", cmd)); !denied {
			t.Errorf("a publish hidden in %q was allowed", cmd)
		}
	}
}

// ⛔ A gate that cries wolf gets disabled. Naming a gated program is not running it.
func TestNamingAGatedProgramIsNotRunningIt(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`go test ./cmd/ghmerge`, `go build ./cmd/ghrelease`, `go build -o /x/gitpush ./cmd/gitpush`,
		`cat ~/.local/bin/gitpush`, `echo ghmerge`, `ls ~/.local/bin/ghpr`, `which gitpush`,
		`git log -- cmd/ghrun`, `grep -rn ghissue .`, `man ghrel`,
		`echo "ghmerge is the tool"`, `x=ghmerge`,
		`for i in ghmerge ghpr; do echo $i; done`,
	} {
		if denied, reason, errOut := ask(env, payload(t, me, "/", cmd)); denied || errOut != "" {
			t.Errorf("%q: denied=%v %s stderr=%q", cmd, denied, reason, errOut)
		}
	}
}

// The gate guards against mistakes, not adversaries: obfuscation is not read.
func TestObfuscationIsDeliberatelyNotRead(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{`'ghmerge' go-numerics/optimize 62`, `ghm\erge go-numerics/optimize 62`, `$x go-numerics/optimize 62`} {
		if denied, _, _ := ask(env, payload(t, me, "/", cmd)); denied {
			t.Errorf("%q is documented as not read, yet it was denied", cmd)
		}
	}
}
