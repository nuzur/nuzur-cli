package main

import (
	"context"

	"example.com/app/modules/season"
	"example.com/app/modules/show"
	"example.com/app/router"
	"example.com/app/store"
	"go.uber.org/fx"
)

func NewServer(lc fx.Lifecycle) int {
	lc.Append(fx.Hook{OnStart: func(context.Context) error { return nil }})
	return 0
}

func main() {
	fx.New(fx.Provide(season.New, show.New, router.New, store.New, NewServer))
}
