package filetools

import (
	"log"
	"os"
)

func CurrentPath() string {
	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	return dir
}
