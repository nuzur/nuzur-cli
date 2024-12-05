package auth

import "fmt"

func (c *AuthClientImplementation) Login() error {
	c.closeApp.Add(1)
	url := fmt.Sprintf("%v/realms/%v/protocol/openid-connect/auth?client_id=%v&redirect_uri=%v&response_type=code",
		c.config.KeycloakConfig.URL,
		c.config.KeycloakConfig.Realm,
		c.config.KeycloakConfig.ClientID,
		c.config.AuthCallbackServer.GetCallbackURL())

	c.startServer()
	OpenBrowser(url)
	c.closeApp.Wait()

	return nil
}
