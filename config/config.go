package config

import (
	"bytes"
	"embed"
	"fmt"

	_ "embed"

	"go.uber.org/config"
)

//go:embed base.yaml
var baseyaml embed.FS

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
	file, err := baseyaml.ReadFile("base.yaml")
	if err != nil {
		return nil, err
	}
	cp, err := config.NewYAML(
		config.RawSource(bytes.NewReader(file)),
	)

	if err != nil {
		return nil, err
	}
	return cp, nil
}
