// Package chi is a minimal stand-in for go-chi used by scanner tests.
package chi

import "net/http"

type Router interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
	HandleFunc(pattern string, h http.HandlerFunc)
	Route(pattern string, fn func(r Router)) Router
	Mount(pattern string, h http.Handler)
}

type Mux struct{}

func NewRouter() *Mux { return &Mux{} }

func (m *Mux) Get(pattern string, h http.HandlerFunc)         {}
func (m *Mux) Post(pattern string, h http.HandlerFunc)        {}
func (m *Mux) HandleFunc(pattern string, h http.HandlerFunc)  {}
func (m *Mux) Route(pattern string, fn func(r Router)) Router { return m }
func (m *Mux) Mount(pattern string, h http.Handler)           {}
