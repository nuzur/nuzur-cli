package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/nuzur/nuzur-cli/config"
	"github.com/nuzur/nuzur-cli/filetools"
)

func (c *AuthClientImplementation) startServer() {
	serverAddress := fmt.Sprintf("localhost:%v", c.config.AuthCallbackServer.Port)
	http.HandleFunc(fmt.Sprintf("/%s", c.config.AuthCallbackServer.CallbackPath), func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			request, err := BuildTokenExchangeRequest(c.config, code)
			if err == nil {
				var resp *http.Response
				var body []byte
				resp, err = http.DefaultClient.Do(request)
				if err == nil {
					body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
					if err != nil {
						fmt.Printf("error reading response: %v", err)
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
							}
							c.closeApp.Done()
						default:
							fmt.Println("Error processing token")
							c.closeApp.Done()
						}
					} else {
						// Error - Invalid status code
					}
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					fmt.Fprintf(w, `You can close this page. The token is in your CLI logs...
						<script>
						setTimeout(()=>{window.close()}, 2000);
						</script>`)
				} else {
					// Error - Could not send the request
				}
			} else {
				// Error - Could not build the Request object
			}
		}
	})

	go func() {
		if err := http.ListenAndServe(serverAddress, nil); err != nil {
			log.Fatalf("Unable to start server: %v\n", err)
			c.closeApp.Done()
		}
	}()
}

func BuildTokenExchangeRequest(config config.Config, code string) (*http.Request, error) {
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
