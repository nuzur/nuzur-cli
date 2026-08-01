package app

import (
	"strings"
	"testing"
)

// urfave/cli v1 runs on the stdlib flag package, which stops parsing at the first
// non-flag argument. Nothing read the leftovers, so `--custom false
// --allow-destructive` set --custom to TRUE (the opposite of what was typed), threw
// `false` away as a positional, and never saw --allow-destructive at all. The whole
// value of this check is that the flags to the right of the stray argument are named,
// because nothing else in the run would ever mention them.
func TestStrayArgsError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		takes     string
		stray     []string
		argv      []string
		wantNil   bool
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:    "no stray arguments is not an error",
			command: "deploy", takes: "takes no positional arguments",
			stray: nil, argv: []string{"nuzur-cli", "deploy", "--host", "prod"},
			wantNil: true,
		},
		{
			// The reported combination, and the dangerous one: the safety flag is the
			// thing that got dropped.
			name:    "--custom false --allow-destructive names the flag and what was lost",
			command: "deploy", takes: "takes no positional arguments",
			stray: []string{"false", "--allow-destructive"},
			argv:  []string{"nuzur-cli", "deploy", "--identifier", "r6box", "--custom", "false", "--allow-destructive"},
			wantIn: []string{
				"`nuzur-cli deploy` takes no positional arguments",
				`"false", "--allow-destructive"`,
				"Did you mean `--custom=false`?",
				"sets --custom to TRUE",
				"Flag parsing stops at the first positional argument",
				`so these were NOT applied: "--allow-destructive"`,
			},
		},
		{
			// The billing version of the same mistake.
			name:    "--custom false --new-vm loses the flag that starts a second bill",
			command: "deploy", takes: "takes no positional arguments",
			stray:  []string{"false", "--new-vm"},
			argv:   []string{"nuzur-cli", "deploy", "--custom", "false", "--new-vm"},
			wantIn: []string{"Did you mean `--custom=false`?", `"--new-vm"`},
		},
		{
			name:    "other bool spellings get the same hint",
			command: "deploy", takes: "takes no positional arguments",
			stray:  []string{"0"},
			argv:   []string{"nuzur-cli", "deploy", "--db-only", "0"},
			wantIn: []string{"Did you mean `--db-only=0`?"},
		},
		{
			// A stray that is not a bool value: still an error, still says why the
			// flags after it did nothing, but does not invent a flag it came from.
			name:    "a plain stray argument is still refused",
			command: "deploy", takes: "takes no positional arguments",
			stray:     []string{"garbagearg"},
			argv:      []string{"nuzur-cli", "deploy", "garbagearg"},
			wantIn:    []string{`"garbagearg"`, "any flag after it would not have been applied"},
			wantNotIn: []string{"Did you mean"},
		},
		{
			// A bool value with no flag before it: the shape is still recognisable, so
			// say the rule without naming a flag that may not be the right one.
			name:    "a bare bool value states the rule without guessing a flag",
			command: "deploy", takes: "takes no positional arguments",
			stray:     []string{"true"},
			argv:      []string{"nuzur-cli", "deploy", "true"},
			wantIn:    []string{"looks like the value of a boolean flag", "`--flag=true`"},
			wantNotIn: []string{"Did you mean"},
		},
		{
			// An inline `--custom=false` is correct, so a stray next to one is a
			// different mistake and must not be blamed on it.
			name:    "an inline bool flag is not blamed",
			command: "deploy", takes: "takes no positional arguments",
			stray:     []string{"false"},
			argv:      []string{"nuzur-cli", "deploy", "--custom=false", "false"},
			wantIn:    []string{"looks like the value of a boolean flag"},
			wantNotIn: []string{"Did you mean"},
		},
		{
			name:    "destroy names the one argument it does take",
			command: "destroy", takes: "takes exactly one positional argument (the deployment id)",
			stray: []string{"extra", "r6box-c3d31228", "--purge"},
			argv:  []string{"nuzur-cli", "destroy", "r6box-c3d31228", "--purge", "extra"},
			wantIn: []string{
				"takes exactly one positional argument (the deployment id), but got 3",
				// All three named, not the tail: urfave reorders the command line, so
				// which of them was meant to be the id is not knowable from here.
				`"extra", "r6box-c3d31228"`,
				`so these were NOT applied: "--purge"`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := strayArgsError(tc.command, tc.takes, tc.stray, tc.argv)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error is missing %q\ngot:\n%s", want, err)
				}
			}
			for _, forbidden := range tc.wantNotIn {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("error should not contain %q\ngot:\n%s", forbidden, err)
				}
			}
		})
	}
}

func TestFlagBefore(t *testing.T) {
	argv := []string{"nuzur-cli", "deploy", "--identifier", "r6box", "--custom", "false", "--new-vm"}
	if got := flagBefore("false", argv); got != "--custom" {
		t.Errorf("flagBefore = %q, want --custom", got)
	}
	// A token that follows another VALUE, not a flag, belongs to nothing.
	if got := flagBefore("--new-vm", argv); got != "" {
		t.Errorf("flagBefore(%q) = %q, want no flag: it follows --custom's stray value", "--new-vm", got)
	}
	// An inline flag already carries its own value, so it is never the answer.
	if got := flagBefore("false", []string{"nuzur-cli", "deploy", "--custom=false", "false"}); got != "" {
		t.Errorf("flagBefore after an inline flag = %q, want empty", got)
	}
	if got := flagBefore("nothere", argv); got != "" {
		t.Errorf("flagBefore of an absent value = %q, want empty", got)
	}
}
