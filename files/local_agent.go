package files

import (
	"errors"
	"os"
	"path"
	"path/filepath"

	"github.com/nuzur/nuzur-cli/constants"
)

// agentDir returns the persistent per-user directory for agent metadata. On
// macOS this is ~/Library/Application Support/nuzur/agent, on Linux it's
// $XDG_CONFIG_HOME/nuzur/agent (defaulting to ~/.config/nuzur/agent).
//
// Falls back to /tmp/nuzur-cli/ when UserConfigDir fails — preserves the
// pre-keychain behavior so a misconfigured environment still functions, at
// the cost of losing data across reboots.
func agentDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "nuzur", "agent")
	}
	return path.Join("/tmp", "nuzur-cli")
}

// legacyAgentDir returns the path the CLI used before relocation. Kept here
// so a single Migrate() call can move state forward without bricking users
// who upgrade with an existing pairing.
func legacyAgentDir() string {
	return path.Join("/tmp", "nuzur-cli")
}

// LocalAgentUUIDFilePath is where the CLI stores the paired local agent's UUID.
func LocalAgentUUIDFilePath() string {
	return filepath.Join(agentDir(), constants.LOCAL_AGENT_UUID_FILE)
}

// LocalAgentTokenFilePath is where the CLI stores the paired local agent's
// plaintext token. (Keychain move tracked separately; phase-keychain-1 only
// covered DSNs.)
func LocalAgentTokenFilePath() string {
	return filepath.Join(agentDir(), constants.LOCAL_AGENT_TOKEN_FILE)
}

// LocalAgentDSNFilePath is where the CLI persists the legacy fallback DSN
// the user supplied on first `agent start`. Held in plaintext for now;
// the per-connection registry's DSNs live in the OS keychain.
func LocalAgentDSNFilePath() string {
	return filepath.Join(agentDir(), constants.LOCAL_AGENT_DSN_FILE)
}

// LocalAgentDriverFilePath stores the driver name (mysql / postgres) that
// pairs with the saved DSN.
func LocalAgentDriverFilePath() string {
	return filepath.Join(agentDir(), constants.LOCAL_AGENT_DRIVER_FILE)
}

// LocalAgentConnectionsFilePath stores the per-connection registry: a JSON
// array of {uuid, name, driver, db_type, default_schema}. DSNs are NOT in
// this file — they live in the OS keychain.
func LocalAgentConnectionsFilePath() string {
	return filepath.Join(agentDir(), constants.LOCAL_AGENT_CONNECTIONS_FILE)
}

// MigrateLegacyAgentFiles relocates any files that still live at the old
// /tmp/nuzur-cli/ path. Called once at the start of every agent-related
// command so users who upgrade past this change don't have to re-pair.
// Idempotent: if the new location already has a file, the legacy copy is
// left in place untouched (we never overwrite).
func MigrateLegacyAgentFiles() error {
	legacy := legacyAgentDir()
	if legacy == agentDir() {
		// UserConfigDir fell back to /tmp; nothing to move.
		return nil
	}
	if _, err := os.Stat(legacy); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(agentDir(), 0o700); err != nil {
		return err
	}
	names := []string{
		constants.LOCAL_AGENT_UUID_FILE,
		constants.LOCAL_AGENT_TOKEN_FILE,
		constants.LOCAL_AGENT_DSN_FILE,
		constants.LOCAL_AGENT_DRIVER_FILE,
		constants.LOCAL_AGENT_CONNECTIONS_FILE,
	}
	for _, name := range names {
		src := filepath.Join(legacy, name)
		dst := filepath.Join(agentDir(), name)
		if _, err := os.Stat(dst); err == nil {
			// New file already exists — don't overwrite.
			continue
		}
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
