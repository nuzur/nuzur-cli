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
