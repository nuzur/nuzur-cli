package auth

import (
	"fmt"
	"os"
	"path"

	"github.com/nuzur/nuzur-cli/constants"
	filetools "github.com/nuzur/nuzur-cli/file_tools"
	outputtools "github.com/nuzur/nuzur-cli/output_tools"
)

type LoginParams struct {
	LoggedIn  string
	LoggedOut string
	Error     string
}

func (c *AuthClientImplementation) Login(params LoginParams) error {
	if filetools.FileExists(path.Join(filetools.CurrentPath(), constants.TOKEN_FILE)) {
		err := c.LoginStatus(params)
		if err == nil {
			return nil
		}
	}

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

	return c.LoginStatus(params)

}

func (c *AuthClientImplementation) LoginStatus(params LoginParams) error {
	if !filetools.FileExists(path.Join(filetools.CurrentPath(), constants.TOKEN_FILE)) {
		outputtools.PrintlnColored(params.LoggedOut, outputtools.Red)
		return nil
	}
	user, err := c.GetTokenUser()
	if err != nil {
		outputtools.PrintlnColored(params.Error, outputtools.Red)
		return err
	}

	finalSuccessMsg := fmt.Sprintf("%s [%s - %s]", params.LoggedIn, user.Name, user.Email)
	outputtools.PrintlnColored(finalSuccessMsg, outputtools.Green)
	return nil
}

func (c *AuthClientImplementation) Logout(loggedoutMsg string) error {
	err := os.Remove(filetools.TokenFilePath())
	outputtools.PrintlnColored(loggedoutMsg, outputtools.Green)
	return err
}
