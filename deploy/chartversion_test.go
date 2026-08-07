package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextChartVersion(t *testing.T) {
	for _, tc := range []struct {
		prior string
		want  string
	}{
		{"", firstChartVersion},              // nothing released yet
		{"0.1.0", "0.1.1"},                   // ordinary bump
		{"1.2.9", "1.2.10"},                  // no decimal weirdness at 9→10
		{"0.0.1", "0.0.2"},                   // the generator's unstamped default
		{"  1.0.0  ", "1.0.1"},               // tolerates surrounding space
		{"not-a-version", firstChartVersion}, // never propagates garbage
		{"1.2", firstChartVersion},           // partial semver is not a version
	} {
		if got := NextChartVersion(tc.prior); got != tc.want {
			t.Errorf("NextChartVersion(%q) = %q, want %q", tc.prior, got, tc.want)
		}
	}
}

// TestNextChartVersionAlwaysAdvances is the property that matters: the chart
// version is stamped into pod template labels, so it is what rolls the pods
// when the image reference has not changed.
func TestNextChartVersionAlwaysAdvances(t *testing.T) {
	v := ""
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		v = NextChartVersion(v)
		if seen[v] {
			t.Fatalf("version %q repeated after %d bumps", v, i)
		}
		seen[v] = true
	}
}

func writeChart(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Chart.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStampChartVersion(t *testing.T) {
	path := writeChart(t, `apiVersion: v2
name: myapp
description: myapp backend service

type: application

# Bump on every release.
version: 0.0.1

appVersion: "0.0.1"
`)
	if err := StampChartVersion(path, "1.4.2"); err != nil {
		t.Fatalf("StampChartVersion: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)

	if !strings.Contains(body, "\nversion: 1.4.2\n") {
		t.Errorf("version not stamped:\n%s", body)
	}
	// Quoted: Helm reads an unquoted 1.10 as a float and turns it into 1.1.
	if !strings.Contains(body, "appVersion: \"1.4.2\"") {
		t.Errorf("appVersion not stamped and quoted:\n%s", body)
	}
	// The surrounding file is left alone — this is a file the user commits, and
	// a reformatted diff is an unreviewable one.
	for _, want := range []string{"apiVersion: v2", "name: myapp", "# Bump on every release."} {
		if !strings.Contains(body, want) {
			t.Errorf("stamping rewrote unrelated content, lost %q:\n%s", want, body)
		}
	}
}

// TestStampChartVersionLeavesDependencyVersions is the whole reason the regexes
// are anchored to the start of a line. A dependency's indented `version:` is
// somebody else's chart and must not be rewritten.
func TestStampChartVersionLeavesDependencyVersions(t *testing.T) {
	path := writeChart(t, `apiVersion: v2
name: sfapi

dependencies:
  - name: sfauthserver
    version: 0.0.4
    repository: oci://ghcr.io/mklfarha/helm

version: 0.0.13
appVersion: "0.0.13"
`)
	if err := StampChartVersion(path, "0.1.0"); err != nil {
		t.Fatalf("StampChartVersion: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "    version: 0.0.4") {
		t.Errorf("dependency version was rewritten:\n%s", body)
	}
	if !strings.Contains(string(body), "\nversion: 0.1.0\n") {
		t.Errorf("chart version not stamped:\n%s", body)
	}
}

// TestReadChartVersionIgnoresDependencies covers the same trap on the read
// side — the one the publish workflow's `grep version: | tail -n1` falls into
// the moment a dependencies block moves below the chart version.
func TestReadChartVersionIgnoresDependencies(t *testing.T) {
	path := writeChart(t, `apiVersion: v2
name: sfapi
version: 0.0.13
appVersion: "0.0.13"

dependencies:
  - name: sfauthserver
    version: 0.0.4
`)
	got, err := ReadChartVersion(path)
	if err != nil {
		t.Fatalf("ReadChartVersion: %v", err)
	}
	if got != "0.0.13" {
		t.Errorf("ReadChartVersion = %q, want the CHART's version 0.0.13", got)
	}
}

func TestStampChartVersionRejectsAChartWithNoVersion(t *testing.T) {
	path := writeChart(t, "apiVersion: v2\nname: myapp\n")
	if err := StampChartVersion(path, "1.0.0"); err == nil {
		t.Fatal("expected an error for a Chart.yaml with no version line")
	}
}
