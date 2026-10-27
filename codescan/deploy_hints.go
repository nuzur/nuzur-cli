package codescan

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"gopkg.in/yaml.v2"
)

var (
	hpaKind      = regexp.MustCompile(`(?m)^\s*kind:\s*HorizontalPodAutoscaler\s*$`)
	deployKind   = regexp.MustCompile(`(?m)^\s*kind:\s*(Deployment|StatefulSet)\s*$`)
	replicasLine = regexp.MustCompile(`(?m)^\s*replicas:\s*(\d+)\s*$`)
	// A helm template wrapped in {{ if .Values.x.enabled }} renders nothing
	// when the value is off, which is how `helm create` ships its HPA.
	helmGuard = regexp.MustCompile(`^\s*\{\{-?\s*if\s+\.Values\.([A-Za-z0-9_.]+)\s*-?\}\}`)
)

// deployHints looks for Kubernetes manifests (plain or helm templates) that
// run more than one replica. With several replicas every in-process ticker
// and startup goroutine runs once per replica, which the server reports.
func deployHints(repoRoot string, rel func(string) string) []*pb.CodeDeployHint {
	var hints []*pb.CodeDeployHint
	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != repoRoot && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 256*1024 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		values, guarded := helmValues(path, raw)
		if guarded && values == nil {
			return nil // guard is off (or unresolvable): the template renders nothing
		}
		if hpaKind.Match(raw) {
			hint := &pb.CodeDeployHint{Kind: "autoscaler", File: rel(path)}
			if max, ok := lookupValue(values, "autoscaling.maxReplicas"); ok {
				hint.Value = max
			}
			hints = append(hints, hint)
		}
		if deployKind.Match(raw) {
			for _, m := range replicasLine.FindAllSubmatch(raw, -1) {
				if n, _ := strconv.Atoi(string(m[1])); n > 1 {
					hints = append(hints, &pb.CodeDeployHint{Kind: "replicas", File: rel(path), Value: string(m[1])})
					break
				}
			}
		}
		return nil
	})
	return hints
}

// helmValues handles a template guarded by {{ if .Values.<path> }}: it reads
// the chart's values.yaml and returns it when the guard is truthy, nil when
// it is not. guarded is false for plain manifests.
func helmValues(path string, raw []byte) (map[interface{}]interface{}, bool) {
	m := helmGuard.FindSubmatch(raw)
	if m == nil {
		return nil, false
	}
	chartDir := filepath.Dir(path)
	if filepath.Base(chartDir) == "templates" {
		chartDir = filepath.Dir(chartDir)
	}
	valuesRaw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		return nil, true
	}
	var values map[interface{}]interface{}
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		return nil, true
	}
	v, ok := lookupValue(values, string(m[1]))
	if !ok || v == "false" || v == "" || v == "0" {
		return nil, true
	}
	return values, true
}

// lookupValue walks a dotted path through parsed YAML and renders the leaf.
func lookupValue(values map[interface{}]interface{}, dotted string) (string, bool) {
	var cur interface{} = values
	for _, key := range strings.Split(dotted, ".") {
		m, ok := cur.(map[interface{}]interface{})
		if !ok {
			return "", false
		}
		if cur, ok = m[key]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil, map[interface{}]interface{}, []interface{}:
		return "", cur != nil
	case bool:
		return strconv.FormatBool(v), true
	case int:
		return strconv.Itoa(v), true
	case string:
		return v, true
	default:
		return "", true
	}
}
