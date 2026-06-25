package files

import (
	"path/filepath"

	"github.com/nuzur/nuzur-cli/constants"
)

// TokenFilePath is where the CLI stores the user's auth (login) token. It lives
// in the persistent per-user config dir — $XDG_CONFIG_HOME/nuzur on Linux,
// ~/Library/Application Support/nuzur on macOS, %AppData%\nuzur on Windows —
// falling back to /tmp/nuzur-cli when UserConfigDir is unavailable. Previously
// this was hardcoded under /tmp, which is wiped on reboot and resolves to a
// drive-root path on Windows; see MigrateLegacyTokenFile for the one-time move.
func TokenFilePath() string {
	return filepath.Join(configBaseDir(), constants.TOKEN_FILE)
}
