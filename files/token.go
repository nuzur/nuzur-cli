package files

import (
	"path"

	"github.com/nuzur/nuzur-cli/constants"
)

func TokenFilePath() string {
	return path.Join("/tmp", "nuzur-cli", constants.TOKEN_FILE)
}
