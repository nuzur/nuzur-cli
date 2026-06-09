package files

import (
	"path"

	"github.com/nuzur/nuzur-cli/constants"
)

// LocalAgentUUIDFilePath is where the CLI stores the paired local agent's UUID.
func LocalAgentUUIDFilePath() string {
	return path.Join("/tmp", "nuzur-cli", constants.LOCAL_AGENT_UUID_FILE)
}

// LocalAgentTokenFilePath is where the CLI stores the paired local agent's
// plaintext token. Phase 2 will move this to the OS keychain.
func LocalAgentTokenFilePath() string {
	return path.Join("/tmp", "nuzur-cli", constants.LOCAL_AGENT_TOKEN_FILE)
}

// LocalAgentDSNFilePath is where the CLI persists the DSN the user supplied
// on first `agent start`. Held in plaintext for phase 2; moves to keychain
// in phase 3 when the per-connection registry lands.
func LocalAgentDSNFilePath() string {
	return path.Join("/tmp", "nuzur-cli", constants.LOCAL_AGENT_DSN_FILE)
}

// LocalAgentDriverFilePath stores the driver name (mysql / postgres) that
// pairs with the saved DSN.
func LocalAgentDriverFilePath() string {
	return path.Join("/tmp", "nuzur-cli", constants.LOCAL_AGENT_DRIVER_FILE)
}
