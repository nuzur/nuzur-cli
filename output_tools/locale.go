package outputtools

import (
	"os"
	"os/exec"
	"slices"
	"strings"
)

var supportedLocales = []string{"en", "es"}

const defaultLocale = "en"

func GetLocale() string {
	envlang, ok := os.LookupEnv("LANG")
	if ok {
		dotsplit := strings.Split(envlang, ".")[0]
		finalLocale := strings.Split(dotsplit, "_")[0]
		if !slices.Contains(supportedLocales, finalLocale) {
			return defaultLocale
		}
		return finalLocale
	}

	// Exec powershell Get-Culture on Windows.
	cmd := exec.Command("powershell", "Get-Culture | select -exp Name")
	output, err := cmd.Output()
	if err == nil {
		trimRes := strings.Trim(string(output), "\r\n")
		finalLocale := strings.Split(trimRes, "_")[0]
		if !slices.Contains(supportedLocales, finalLocale) {
			return defaultLocale
		}
		return finalLocale
	}

	return "en"
}
