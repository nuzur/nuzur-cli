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

// page is loaded by a name joined at run time; the scanner finds the one
// template whose path ends with it.
func page(name string) string { return "./templates/" + name }

var showPage = page("show.html")

// footerScripts are loaded by footer.html as /assets/js/{{$f}}.js.
var footerScripts = []string{"footer_widget"}
