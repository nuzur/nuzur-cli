// Package season serves season pages.
//
// Second paragraph is not part of the summary.
package season

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"example.com/app/sfapi/gen"
	"example.com/app/store"
	"go.uber.org/fx"
)

const apiBase = "https://api.partner.net/v1"

type Params struct {
	fx.In
	Store *store.Client
	API   gen.SfapiClient
}

type Implementation struct {
	store *store.Client
	api   gen.SfapiClient
}

func New(p Params) *Implementation {
	i := &Implementation{store: p.Store, api: p.API}
	go i.refresh()
	return i
}

func (i *Implementation) refresh() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
}

func (i *Implementation) Handler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	_ = i.api.ListSeason(ctx)
	_ = i.api.ListEpisodeHasDrag(ctx)
	_ = i.api.Ping(ctx)
	_, _ = i.store.Get(ctx, "season:all")
	_ = i.store.SetRaw(ctx, fmt.Sprintf("season:%s:views", "x"), "1")
}

func (i *Implementation) Reset(w http.ResponseWriter, r *http.Request) {
	_ = i.api.UpdateSeason(context.Background())
}
