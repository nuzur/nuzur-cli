package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RunScript's error is the only thing most failures are ever described by: it
// is what the terminal prints, and what lands in the deployment record's
// last_error for the NEXT run to read back. Two things about it are load-bearing.
//
//   - It must name the script that failed. One runner serves the deploy's
//     bootstrap and destroy's teardown, and a hardcoded noun meant destroying a
//     dead box reported `remote bootstrap script failed` — a step that does not
//     exist in a destroy, sending the reader to look for a deploy that never ran.
//   - It must carry the CAUSE. ssh writes its diagnosis
//     ("...Operation timed out") to its own stderr; replacing it with
//     `exit status 255` throws away the only sentence that says what happened.

// fakeSSH puts an `ssh` on PATH that echoes a scripted stderr and exits with a
// scripted status, so RunScript can be exercised end to end with no host.
func fakeSSH(t *testing.T, stderr string, exit int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n"
	if stderr != "" {
		script += "printf '%s' " + shellSingleQuote(stderr) + " >&2\n"
	}
	script += "exit " + itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runScriptWith(t *testing.T, label string) error {
	t.Helper()
	r := NewSSHRunner(Target{Host: "203.0.113.10"})
	r.Stderr = &bytes.Buffer{}
	return r.RunScript(context.Background(), label, "echo hi\n")
}

// The failure is named after the script the CALLER ran, not after the bootstrap.
func TestRunScriptNamesTheScriptThatFailed(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  string
		notIn string
	}{
		{ScriptBootstrap, "remote bootstrap script failed", "teardown"},
		{ScriptTeardown, "remote teardown script failed", "bootstrap"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			fakeSSH(t, "", 1)
			err := runScriptWith(t, tc.label)
			if err == nil {
				t.Fatal("a non-zero remote exit returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.notIn) {
				t.Errorf("error = %q names %q, which no %s runs", err, tc.notIn, tc.label)
			}
		})
	}
}

// The reason survives into the error rather than staying on the terminal, and
// the exit error stays unwrapped underneath for errors.As.
func TestRunScriptCarriesTheStderrCause(t *testing.T) {
	fakeSSH(t, "ssh: connect to host 203.0.113.10 port 22: Operation timed out\n", 255)
	err := runScriptWith(t, ScriptTeardown)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"remote teardown script failed",
		"exit status 255",
		"ssh: connect to host 203.0.113.10 port 22: Operation timed out",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("error = %q does not wrap the exec failure; a caller cannot inspect the exit code", err)
	}
}

// Whatever the box printed still reaches the user live — the tail is a copy,
// not a diversion. This is also the regression pin for the aliasing trap: give
// os/exec two DIFFERENT writers over one bytes.Buffer and its io.Copy ReadFrom
// path quietly eats the output, so this sink comes back empty.
func TestRunScriptStillStreamsWhatItQuotes(t *testing.T) {
	fakeSSH(t, "E: The package cache file is corrupted\n", 100)
	var sink bytes.Buffer
	r := NewSSHRunner(Target{Host: "203.0.113.10"})
	r.Stderr = &sink
	err := r.RunScript(context.Background(), ScriptBootstrap, "echo hi\n")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(sink.String(), "E: The package cache file is corrupted") {
		t.Errorf("the live stream was swallowed: %q", sink.String())
	}
	// ...and the same line is the diagnosis in the error.
	if !strings.Contains(err.Error(), "E: The package cache file is corrupted") {
		t.Errorf("error = %q, want the apt failure quoted", err)
	}
}

func TestRunScriptSucceedsQuietly(t *testing.T) {
	fakeSSH(t, "", 0)
	if err := runScriptWith(t, ScriptBootstrap); err != nil {
		t.Errorf("RunScript on a clean run: %v", err)
	}
}

// A bootstrap streams an entire docker build through stderr. The tail must stay
// small, and must not quote the mid-line fragment its own window starts on.
func TestTailBufferKeepsOnlyTheEnd(t *testing.T) {
	tb := &tailBuffer{max: 16}
	if _, err := tb.Write([]byte(strings.Repeat("a", 100) + "END")); err != nil {
		t.Fatal(err)
	}
	if got := tb.String(); len(got) != 16 || !strings.HasSuffix(got, "END") {
		t.Errorf("tail = %q (%d bytes), want the last 16 ending in END", got, len(got))
	}
}

func TestLastLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"blank lines are skipped", "a\n\nb\n\n", "a; b"},
		{"only the last two", "one\ntwo\nthree\nfour\n", "three; four"},
		{"nothing to say", "\n  \n", ""},
		{"a single line", "boom\n", "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastLines(tc.in, 2); got != tc.want {
				t.Errorf("lastLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// One enormous line must not turn a one-line error into a paragraph.
	if got := lastLines(strings.Repeat("x", 900), 2); len(got) > 405 {
		t.Errorf("lastLines did not cap a pathological line: %d bytes", len(got))
	}
}
