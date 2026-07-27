package app

import (
	"fmt"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/nuzur/nuzur-cli/extensionrun"
)

// Some capabilities ship as a pair of backend extensions that do the same job
// over a different connection path: sql-push reaches the database directly from
// nuzur, sql-push-local reaches it through an agent running next to it. The pair
// members take different config fields, but choosing between them is a property
// of the user's setup, not a separate feature — so the CLI (like the web editor)
// offers one entry, asks for the connection mode, and runs the matching member.
type connectionMode string

const (
	connModeRemote connectionMode = "remote" // "Direct"
	connModeLocal  connectionMode = "local"  // "Via agent"
)

// parseConnectionMode accepts both the internal names and the words the UI uses,
// so --connection-mode direct and --connection-mode remote are the same thing.
func parseConnectionMode(s string) (connectionMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "remote", "direct":
		return connModeRemote, nil
	case "local", "agent":
		return connModeLocal, nil
	default:
		return "", fmt.Errorf("invalid connection mode %q: use 'remote' (direct) or 'local' (via agent)", s)
	}
}

// extensionPair maps one user-facing extension to its two backend members.
// Front doubles as the remote member's identifier and as what the picker shows.
type extensionPair struct {
	Front string
	Local string
}

var extensionPairs = []extensionPair{
	{Front: "sql-push", Local: "sql-push-local"},
	{Front: "sql-import", Local: "sql-import-local"},
}

func (p extensionPair) memberForMode(m connectionMode) string {
	if m == connModeLocal {
		return p.Local
	}
	return p.Front
}

// modeForMember reports which mode runs the given member of this pair.
func (p extensionPair) modeForMember(identifier string) (connectionMode, bool) {
	switch identifier {
	case p.Front:
		return connModeRemote, true
	case p.Local:
		return connModeLocal, true
	default:
		return "", false
	}
}

// pairForIdentifier finds the pair an identifier belongs to, whether the caller
// named the front or the agent-side member directly.
func pairForIdentifier(identifier string) (extensionPair, bool) {
	for _, p := range extensionPairs {
		if _, ok := p.modeForMember(identifier); ok {
			return p, true
		}
	}
	return extensionPair{}, false
}

func pairFrontIdentifiers() []string {
	fronts := make([]string, 0, len(extensionPairs))
	for _, p := range extensionPairs {
		fronts = append(fronts, p.Front)
	}
	return fronts
}

// mustPairForFront looks up a pair by its front identifier. It panics on an
// unknown front because callers pass compile-time constants.
func mustPairForFront(front string) extensionPair {
	for _, p := range extensionPairs {
		if p.Front == front {
			return p
		}
	}
	panic("unknown extension pair front: " + front)
}

// inferModeFromConfig reads the mode out of the shape of a config: the two
// members take disjoint connection fields, so the keys say which backend the
// config was written for. Matches how the web app infers mode from saved values.
func inferModeFromConfig(cfg map[string]interface{}) (connectionMode, bool) {
	if len(cfg) == 0 {
		return "", false
	}
	if _, ok := cfg["local_agent"]; ok {
		return connModeLocal, true
	}
	if _, ok := cfg["connection"]; ok {
		return connModeRemote, true
	}
	if _, ok := cfg["store"]; ok {
		return connModeRemote, true
	}
	return "", false
}

// decidePairMember resolves which member of a pair to run, in precedence order:
//
//  1. the caller named the agent-side member outright (--extension sql-push-local),
//  2. --connection-mode,
//  3. the shape of a supplied --config,
//  4. interactively: ask, seeded with the mode used last,
//  5. non-interactively: remote, so scripted runs stay deterministic.
//
// When needPrompt is true the caller must ask and use the answer; seed is the
// mode the prompt should start on.
func decidePairMember(
	pair extensionPair,
	requested string,
	modeFlag string,
	providedCfg map[string]interface{},
	entries map[string]extensionrun.LastUsedEntry,
	interactive bool,
) (identifier string, needPrompt bool, seed connectionMode, err error) {
	var flagMode connectionMode
	if modeFlag != "" {
		flagMode, err = parseConnectionMode(modeFlag)
		if err != nil {
			return "", false, "", err
		}
	}

	// An explicit member identifier is itself a mode choice; a --connection-mode
	// that disagrees is a contradiction rather than something to silently pick.
	if requestedMode, ok := pair.modeForMember(requested); ok && requested != pair.Front {
		if flagMode != "" && flagMode != requestedMode {
			return "", false, "", fmt.Errorf(
				"--extension %s runs in %s mode, which conflicts with --connection-mode %s; drop one of the two",
				requested, requestedMode, modeFlag)
		}
		return requested, false, requestedMode, nil
	}

	if flagMode != "" {
		return pair.memberForMode(flagMode), false, flagMode, nil
	}

	if mode, ok := inferModeFromConfig(providedCfg); ok {
		return pair.memberForMode(mode), false, mode, nil
	}

	seed = seedModeFromLastUsed(pair, entries)
	if interactive {
		return "", true, seed, nil
	}
	return pair.memberForMode(connModeRemote), false, connModeRemote, nil
}

// seedModeFromLastUsed picks the mode of whichever member ran most recently,
// defaulting to remote when neither has been used.
func seedModeFromLastUsed(pair extensionPair, entries map[string]extensionrun.LastUsedEntry) connectionMode {
	remote, hasRemote := entries[pair.Front]
	local, hasLocal := entries[pair.Local]
	switch {
	case hasLocal && !hasRemote:
		return connModeLocal
	case hasRemote && !hasLocal:
		return connModeRemote
	case hasLocal && hasRemote:
		if local.LastUsed.After(remote.LastUsed) {
			return connModeLocal
		}
		return connModeRemote
	default:
		return connModeRemote
	}
}

// aliasAwareLastConfig merges both members' saved configs so the resolved member
// can be seeded whichever way the user ran it last. The connection fields are
// disjoint between members, so they never collide; fields the two share (e.g.
// sql-import's mode and infer_weak_relationships) carry over from the more
// recent run.
func aliasAwareLastConfig(pair extensionPair, entries map[string]extensionrun.LastUsedEntry) map[string]interface{} {
	remote := entries[pair.Front]
	local := entries[pair.Local]

	older, newer := remote, local
	if remote.LastUsed.After(local.LastUsed) {
		older, newer = local, remote
	}

	merged := make(map[string]interface{}, len(older.ConfigValues)+len(newer.ConfigValues))
	for k, v := range older.ConfigValues {
		merged[k] = v
	}
	for k, v := range newer.ConfigValues {
		merged[k] = v
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// filterConfigToFields drops keys the resolved extension doesn't declare. The
// merged pair config carries both members' connection fields, and sending the
// other mode's fields to the backend (which "reuse previous configuration" would
// otherwise do verbatim) is not something it expects.
func filterConfigToFields(cfg map[string]interface{}, fieldIdentifiers []string) map[string]interface{} {
	if len(cfg) == 0 {
		return nil
	}
	known := make(map[string]bool, len(fieldIdentifiers))
	for _, id := range fieldIdentifiers {
		known[id] = true
	}
	filtered := make(map[string]interface{}, len(cfg))
	for k, v := range cfg {
		if known[k] {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// promptConnectionMode asks how the extension should reach the database.
func (i *Implementation) promptConnectionMode(seed connectionMode) (connectionMode, error) {
	items := []string{
		i.localize.Localize("connection_mode_remote", "Direct — nuzur connects to your database over the network"),
		i.localize.Localize("connection_mode_local", "Via agent — through a local agent running next to your database"),
	}
	cursor := 0
	if seed == connModeLocal {
		cursor = 1
	}
	prompt := promptui.Select{
		Label:     i.localize.Localize("connection_mode", "Connection mode"),
		Items:     items,
		CursorPos: cursor,
	}
	idx, _, err := prompt.Run()
	if err != nil {
		return "", err
	}
	if idx == 1 {
		return connModeLocal, nil
	}
	return connModeRemote, nil
}
