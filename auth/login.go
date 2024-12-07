package auth

import (
	"errors"
	"fmt"
	"path"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/filetools"
)

func (c *AuthClientImplementation) Login() error {
	c.closeApp.Add(1)
	url := fmt.Sprintf("%v/realms/%v/protocol/openid-connect/auth?client_id=%v&redirect_uri=%v&response_type=code",
		c.config.KeycloakConfig.URL,
		c.config.KeycloakConfig.Realm,
		c.config.KeycloakConfig.ClientID,
		c.config.AuthCallbackServer.GetCallbackURL())

	c.startServer()
	err := OpenBrowser(url)
	if err != nil {
		return err
	}
	c.closeApp.Wait()
	if !filetools.FileExists(path.Join(filetools.CurrentPath(), constants.TOKEN_FILE)) {
		return errors.New("token file not found")
	}
	return nil
}
