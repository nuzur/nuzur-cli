// Package fx is a minimal stand-in for go.uber.org/fx used by scanner tests.
package fx

import "context"

type In struct{}

type Hook struct {
	OnStart func(context.Context) error
	OnStop  func(context.Context) error
}

type Lifecycle interface{ Append(Hook) }

type Option interface{}

func Provide(constructors ...interface{}) Option { return nil }

func New(opts ...Option) {}
