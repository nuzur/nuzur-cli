package config

import (
	"fmt"
	"path"

	"github.com/nuzur/filetools"
	"go.uber.org/config"
)

type Config struct {
	KeycloakConfig     *KeycloakConfig     `yaml:"keycloak-config"`
	AuthCallbackServer *AuthCallbackServer `yaml:"auth-callback-server"`
}

type KeycloakConfig struct {
	URL      string `yaml:"url"`
	Realm    string `yaml:"realm"`
	ClientID string `yaml:"client-id"`
}

type AuthCallbackServer struct {
	Port         uint32 `yaml:"port"`
	CallbackPath string `yaml:"callback-path"`
}

func (c *AuthCallbackServer) GetCallbackURL() string {
	return fmt.Sprintf("http://localhost:%v/%v", c.Port, c.CallbackPath)
}

func New() (config.Provider, error) {
	pt := filetools.CurrentPath()
	cp, err := config.NewYAML(
		config.File(path.Join(pt, "config", "base.yaml")),
	)

	if err != nil {
		return nil, err
	}
	return cp, nil
}
