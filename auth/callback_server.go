package auth

import (
	"fmt"
	"net/http"
)

func (c *AuthClientImplementation) startServer() {
	serverAddress := fmt.Sprintf("localhost:%v", c.config.AuthCallbackServer.Port)
	http.HandleFunc(fmt.Sprintf("/%s", c.config.AuthCallbackServer.CallbackPath), func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			err := c.FetchToken(code)
			if err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `nuzur login...
						<script>
						setTimeout(()=>{window.close()}, 2000);
						</script>`)
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<b>error:</b> <br/> %v`, err)
			}
			c.closeApp.Done()
		}
	})

	go func() {
		err := http.ListenAndServe(serverAddress, nil)
		if err != nil {
			fmt.Printf("Unable to start server: %v\n", err)
			c.closeApp.Done()
		}
	}()
}
