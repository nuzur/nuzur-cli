package auth

import (
	"errors"
	"fmt"
	"path"

	"github.com/nuzur/nuzur-cli/constants"
	filetools "github.com/nuzur/nuzur-cli/file_tools"
	outputtools "github.com/nuzur/nuzur-cli/output_tools"
)

func (c *AuthClientImplementation) Login(successMsg string, errorMsg string) error {
	c.closeApp.Add(1)
	url := fmt.Sprintf("%v/realms/%v/protocol/openid-connect/auth?client_id=%v&redirect_uri=%v&response_type=code&scope=openid",
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
	user, err := c.GetTokenUser()
	if err != nil {
		outputtools.PrintlnColored(errorMsg, outputtools.Red)
		return err
	}

	finalSuccessMsg := fmt.Sprintf("%s [%s - %s]", successMsg, user.Name, user.Email)
	outputtools.PrintlnColored(finalSuccessMsg, outputtools.Green)
	return nil
}
