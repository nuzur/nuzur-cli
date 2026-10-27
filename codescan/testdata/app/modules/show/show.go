package show

import (
	"net/http"

	"example.com/app/gen/core/module/drag"
	"example.com/app/modules/season"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Season *season.Implementation
	Drags  *drag.Implementation
}

type Implementation struct{ drags *drag.Implementation }

func New(p Params) *Implementation { return &Implementation{drags: p.Drags} }

func (i *Implementation) Handler(w http.ResponseWriter, r *http.Request) {
	_ = i.drags.FetchDragByUUID("x")
	_ = i.drags.Insert()
}
