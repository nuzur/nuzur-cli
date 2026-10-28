package season

import "html/template"

// pageTpl is parsed from a path relative to the module root, the process's
// working directory.
var pageTpl = template.Must(template.ParseFiles("./templates/season/season.html", "./templates/header.html"))
