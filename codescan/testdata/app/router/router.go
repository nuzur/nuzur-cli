package router

import (
	"net/http"

	"example.com/app/modules/season"
	"example.com/app/modules/show"
	"github.com/go-chi/chi/v5"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Season *season.Implementation
	Show   *show.Implementation
}

func auth(h http.HandlerFunc) http.HandlerFunc { return h }

func New(p Params) chi.Router {
	r := chi.NewRouter()
	r.Get("/season/{id}", p.Season.Handler)
	r.HandleFunc("/show", p.Show.Handler)
	r.Route("/admin", func(r chi.Router) {
		r.Post("/reset", auth(p.Season.Reset))
	})
	return r
}
