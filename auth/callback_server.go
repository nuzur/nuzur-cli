package auth

import (
	"bytes"
	"embed"
	"fmt"
	"net/http"
	"text/template"

	"github.com/gorilla/mux"
)

// content holds our static web server content.
//
//go:embed html/assets/img/* html/assets/css/*
//go:embed html/login.html
var content embed.FS

func (c *AuthClientImplementation) startServer() {
	rtr := mux.NewRouter()
	serverAddress := fmt.Sprintf("localhost:%v", c.config.AuthCallbackServer.Port)
	rtr.HandleFunc(fmt.Sprintf("/%s", c.config.AuthCallbackServer.CallbackPath), func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			err := c.FetchToken(code)
			if err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				render, err := c.RenderResponseHTML(true, nil)
				if err == nil && render != nil {
					fmt.Fprintf(w, *render)
				} else {
					fmt.Printf("error: %v", err)
				}

			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				render, err := c.RenderResponseHTML(false, err)
				if err == nil && render != nil {
					fmt.Fprintf(w, *render)
				} else {
					fmt.Printf("error: %v", err)
				}
			}
			c.closeApp.Done()
		}
	})

	rtr.PathPrefix("/html/assets/css").Handler(http.FileServerFS(content))
	rtr.PathPrefix("/html/assets/img").Handler(http.FileServerFS(content))

	go func() {
		err := http.ListenAndServe(serverAddress, rtr)
		if err != nil {
			fmt.Printf("Unable to start server: %v\n", err)
			c.closeApp.Done()
		}
	}()
}

func (c *AuthClientImplementation) RenderResponseHTML(success bool, responseErr error) (*string, error) {

	// read template file
	templateData, err := content.ReadFile("html/login.html")
	if err != nil {
		fmt.Println("reading error", err)
		return nil, err
	}

	// instantiate the template
	t, err := template.New("template").Parse(string(templateData))
	if err != nil {
		return nil, fmt.Errorf("error with provided template: %w", err)
	}

	templateVariables := struct {
		Title    string
		Subtitle string
		Close    string
	}{
		Close: c.localize.Localize("login_close", "Close"),
	}

	if success {
		templateVariables.Title = c.localize.Localize("login_title_success", "Successful Login")
		templateVariables.Subtitle = c.localize.Localize("login_subtitle_success", "You can close this page and return to the terminal")
	} else {
		templateVariables.Title = c.localize.Localize("login_title_error", "Login Error")
		templateVariables.Subtitle = responseErr.Error()
	}

	// execute with data
	var buf bytes.Buffer
	err = t.Execute(&buf, templateVariables)
	if err != nil {
		return nil, fmt.Errorf("error executing template")
	}

	output := buf.String()
	return &output, nil
}
