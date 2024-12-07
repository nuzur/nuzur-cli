package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/nuzur/nuzur-cli/config"
	"github.com/nuzur/nuzur-cli/filetools"
)

func (c *AuthClientImplementation) IsValidToken() (bool, error) {

	return false, nil
}

func (c *AuthClientImplementation) FetchToken(code string) error {
	os.Remove(path.Join(filetools.CurrentPath(), "token.txt"))
	request, err := buildTokenExchangeRequest(c.config, code)
	if err == nil {
		var resp *http.Response
		var body []byte
		resp, err = http.DefaultClient.Do(request)
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				fmt.Printf("error reading response: %v", err)
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				content, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
				switch content {
				case "application/json":
					var f interface{}
					json.Unmarshal(body, &f)
					m := f.(map[string]interface{})

					token := m["access_token"].(string)
					err := filetools.Write("token.txt", []byte(token))
					if err != nil {
						fmt.Printf("error writing token file: %v", err)
						return err
					}
					return nil
				default:
					fmt.Println("Error processing token")
					return errors.New("error processing token")
				}
			} else {
				return errors.New("invalid status")
			}
		} else {
			return err
		}
	} else {
		return err
	}
}

func buildTokenExchangeRequest(config config.Config, code string) (*http.Request, error) {
	tokenURL := fmt.Sprintf("%v/realms/%v/protocol/openid-connect/token", config.KeycloakConfig.URL, config.KeycloakConfig.Realm)

	body := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"client_id":    {config.KeycloakConfig.ClientID},
		"redirect_uri": {config.AuthCallbackServer.GetCallbackURL()},
	}
	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, err
}
