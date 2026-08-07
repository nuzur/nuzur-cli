package deploy

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// firstChartVersion is where a project's chart history starts when nothing has
// been released yet. Not 0.0.1 — that is what the generator emits for an
// unstamped chart, and starting there would make the first real release
// indistinguishable from a chart nobody has deployed.
const firstChartVersion = "0.1.0"

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// NextChartVersion returns the version to stamp into Chart.yaml for this deploy.
//
// The chart version is not decoration. Two things depend on it moving:
//   - The publish workflow names the packaged .tgz after it, so a version that
//     never changes overwrites one mutable artifact in the registry forever.
//   - It is stamped into the pod template labels, which is what actually rolls
//     the pods when the image reference has not changed (a moving tag like
//     :latest resolves to the same string every time).
//
// So it always advances, even when nothing else about the deploy did.
func NextChartVersion(prior string) string {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(prior))
	if m == nil {
		return firstChartVersion
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return firstChartVersion
	}
	return fmt.Sprintf("%s.%s.%d", m[1], m[2], patch+1)
}

var (
	chartVersionLine    = regexp.MustCompile(`(?m)^version:[ \t]*.*$`)
	chartAppVersionLine = regexp.MustCompile(`(?m)^appVersion:[ \t]*.*$`)
)

// StampChartVersion rewrites version and appVersion in a Chart.yaml.
//
// Line-oriented rather than a YAML round-trip, deliberately: re-marshalling
// would reorder keys, drop the comments explaining what the version does, and
// reformat the dependencies block — turning a one-line version bump into an
// unreviewable diff on a file the user commits.
//
// Both are set to the same value. Every chart in use keeps them in lockstep,
// and the publish workflow reads the version back out assuming as much.
func StampChartVersion(chartYAMLPath, version string) error {
	raw, err := os.ReadFile(chartYAMLPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", chartYAMLPath, err)
	}
	content := string(raw)

	if !chartVersionLine.MatchString(content) {
		return fmt.Errorf("%s has no top-level version: line to stamp", chartYAMLPath)
	}
	content = chartVersionLine.ReplaceAllString(content, "version: "+version)

	// appVersion is quoted by convention, and Helm treats an unquoted numeric
	// one as a float — 1.10 would become 1.1.
	if chartAppVersionLine.MatchString(content) {
		content = chartAppVersionLine.ReplaceAllString(content, `appVersion: "`+version+`"`)
	} else {
		content = strings.TrimRight(content, "\n") + "\n\nappVersion: \"" + version + "\"\n"
	}

	if err := os.WriteFile(chartYAMLPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", chartYAMLPath, err)
	}
	return nil
}

// ReadChartVersion returns the top-level version from a Chart.yaml.
//
// Anchored to the start of a line so a dependency's indented `version:` cannot
// be mistaken for the chart's own — the trap the publish workflow's
// `grep version: | tail -n1` walks straight into if the dependencies block is
// ever moved below the version.
func ReadChartVersion(chartYAMLPath string) (string, error) {
	raw, err := os.ReadFile(chartYAMLPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", chartYAMLPath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("%s has no top-level version: line", chartYAMLPath)
}
