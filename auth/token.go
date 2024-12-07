package auth

import (
	"context"
	"fmt"
	"os"

	"github.com/Nerzal/gocloak/v13"
	"github.com/nuzur/filetools"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	nemgen "github.com/nuzur/nuzur-cli/protodeps/nem/idl/gen"
)

func (c *AuthClientImplementation) GetTokenUser() (*nemgen.User, error) {
	pc, err := productclient.New(productclient.Params{})
	if err != nil {
		return nil, err
	}

	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}
	return pc.ProductClient.GetTokenUser(ctx, &gen.GetTokenUserRequest{})
}

func (c *AuthClientImplementation) FetchToken(code string) error {

	os.Remove(files.TokenFilePath())

	client := gocloak.NewClient(c.config.KeycloakConfig.URL)
	grantType := "authorization_code"
	scope := "openid"
	redirect := c.config.AuthCallbackServer.GetCallbackURL()
	token, err := client.GetToken(context.Background(), c.config.KeycloakConfig.Realm, gocloak.TokenOptions{
		ClientID:    &c.config.KeycloakConfig.ClientID,
		GrantType:   &grantType,
		Code:        &code,
		RedirectURI: &redirect,
		Scope:       &scope,
	})

	if err != nil {
		return err
	}

	err = filetools.Write(constants.TOKEN_FILE, []byte(token.AccessToken))
	if err != nil {
		fmt.Printf("error writing token file: %v", err)
		return err
	}

	return nil
}
