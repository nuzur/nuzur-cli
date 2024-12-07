package filetools

import (
	"log"
	"os"
	"path"

	"github.com/nuzur/nuzur-cli/constants"
)

func CurrentPath() string {
	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	return dir
}

func TokenFilePath() string {
	return path.Join(CurrentPath(), constants.TOKEN_FILE)
}
