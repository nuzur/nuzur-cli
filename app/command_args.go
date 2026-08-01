package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli"
)

// command_args.go rejects positional arguments a command does not take.
//
// This is not tidiness. urfave/cli v1 is built on the stdlib `flag` package, which
// STOPS PARSING at the first non-flag argument — everything to its right is handed
// back as positional and never looked at. Nothing in the CLI read those positionals,
// so they were discarded in silence.
//
// The combination that makes it dangerous is a boolean flag written with a space.
// `--custom false` parses as `--custom` (true — the value the user was trying to turn
// OFF) plus a positional `false`, and every flag after it is dropped without a word:
//
//	deploy --identifier r6box --custom false --db postgres --print-config
//
// ignored --db AND --print-config and walked into a real deploy. The two that matter
// most are the two that are hardest to notice missing: `--custom false
// --allow-destructive` silently drops the authorization gate for data loss, and
// `--custom false --new-vm` silently drops the flag that decides whether a second
// server starts billing. `deploy garbagearg` was accepted just as quietly.
//
// The rule is therefore: if a command cannot use a positional argument, getting one
// is always a mistake, and it fails before anything is provisioned.

// boolFlagValues are the spellings the stdlib flag package accepts as a boolean
// value, and so the ones most likely to be sitting in a stray positional.
var boolFlagValues = map[string]bool{
	"true": true, "false": true,
	"1": true, "0": true,
	"t": true, "f": true,
}

// requireNoArgs fails a command that takes no positional arguments at all. Call it
// first in the command's Action, before it logs in or resolves anything.
func requireNoArgs(c *cli.Context, command string) error {
	return strayArgsError(command, "takes no positional arguments", []string(c.Args()), os.Args)
}

// requireOneArg fails a command that takes exactly one positional argument once it
// has been given more.
//
// It names ALL of them, not the tail, because which one was meant is not knowable:
// urfave/cli v1 REORDERS the command line before parsing (flags first, positionals
// after), so `destroy <id> --purge extra` leaves `extra` as Args().First() and the id
// behind it. Naming the leftovers only would point at the id and call it the mistake.
func requireOneArg(c *cli.Context, command, argName string) error {
	args := []string(c.Args())
	if len(args) <= 1 {
		return nil
	}
	return strayArgsError(command,
		fmt.Sprintf("takes exactly one positional argument (%s)", argName),
		args, os.Args)
}

// strayArgsError explains the leftover positional arguments, or returns nil.
//
// argv is the raw command line, used only to name the flag a bool-looking value most
// likely belongs to — the hint that turns "unexpected argument" into something the
// user can act on immediately.
func strayArgsError(command, takes string, stray, argv []string) error {
	if len(stray) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "`nuzur-cli %s` %s, but got %d — %s.", command, takes, len(stray), quotedList(stray))
	if hint := boolFlagHint(stray[0], argv); hint != "" {
		b.WriteString("\n" + hint)
	}
	// Said in every case, because this is the part that costs: the flags to the right
	// of a positional were never parsed, and nothing else would have mentioned them.
	b.WriteString("\nFlag parsing stops at the first positional argument")
	if dropped := flagsAmong(stray[1:]); len(dropped) > 0 {
		fmt.Fprintf(&b, ", so these were NOT applied: %s.", quotedList(dropped))
	} else {
		b.WriteString(", so any flag after it would not have been applied.")
	}
	return errors.New(b.String())
}

// flagsAmong picks out the leftovers that were meant to be flags — the ones whose
// silent loss is the actual cost.
func flagsAmong(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			out = append(out, a)
		}
	}
	return out
}

// boolFlagHint names the flag a bool-looking stray argument was probably meant for.
func boolFlagHint(value string, argv []string) string {
	if !boolFlagValues[strings.ToLower(value)] {
		return ""
	}
	flag := flagBefore(value, argv)
	if flag == "" {
		return fmt.Sprintf("%q looks like the value of a boolean flag. Boolean flags only take a value in the "+
			"`=` form — `--flag=%s`, never `--flag %s`.", value, value, value)
	}
	return fmt.Sprintf("Did you mean `%s=%s`? Boolean flags need the `=` form: `%s %s` sets %s to TRUE and "+
		"leaves %q behind as a positional argument.", flag, value, flag, value, flag, value)
}

// flagBefore returns the flag immediately preceding a value in the raw command line,
// or "". The stdlib parser stops at the first positional, so that value is the first
// stray argument and the token before it is the flag it was meant for.
func flagBefore(value string, argv []string) string {
	for idx := 1; idx < len(argv); idx++ {
		if argv[idx] != value {
			continue
		}
		if prev := argv[idx-1]; strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=") {
			return prev
		}
	}
	return ""
}

// quotedList renders arguments for an error message, quoted so an empty or
// space-containing one is visible.
func quotedList(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, fmt.Sprintf("%q", a))
	}
	return strings.Join(out, ", ")
}
