package files

import (
	"path"

	"github.com/nuzur/filetools"
	"github.com/nuzur/nuzur-cli/constants"
)

func TokenFilePath() string {
	return path.Join(filetools.CurrentPath(), constants.TOKEN_FILE)
}
