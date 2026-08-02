package outputtools

import (
	"fmt"
	"io"
	"os"
)

// Stdout and Stderr are the CLI's two output sinks. Everything the CLI prints
// goes through one of them — the colored helpers below, and the raw fmt.Fprint*
// calls in the command packages — so a test can capture a whole command's output
// by swapping them, and production keeps writing to the real file descriptors.
//
// Package-level rather than plumbed through every signature because the printing
// sites are spread across ~40 call sites in a single linear pipeline; swapping
// two variables is the seam, and it defaults to exactly what the CLI did before.
// Tests that swap them must restore them (and must not run in parallel).
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

type OutputColor int32

const (
	Reset   OutputColor = 0
	Red     OutputColor = 31
	Green   OutputColor = 32
	Yellow  OutputColor = 33
	Blue    OutputColor = 34
	Magenta OutputColor = 35
	Cyan    OutputColor = 36
	Gray    OutputColor = 37
	White   OutputColor = 97
)

func PrintlnColored(text string, color OutputColor) {
	colored := fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, text)
	fmt.Fprintln(Stdout, colored)
}

// PrintlnColoredErr writes a colored line to stderr. Use it for progress/status
// messages so that stdout stays clean for machine-readable (--json) output.
func PrintlnColoredErr(text string, color OutputColor) {
	colored := fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, text)
	fmt.Fprintln(Stderr, colored)
}
