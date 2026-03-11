package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
)

//go:embed templates/page.html
var pageHTML string

//go:embed static
var staticFS embed.FS

func staticSubFS() (fs.FS, error) {
	return fs.Sub(staticFS, "static")
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"usd": func(f float64) string {
		if f == 0 {
			return "--"
		}
		if f >= 1000000 {
			return fmt.Sprintf("$%.1fM", f/1000000)
		}
		if f >= 1000 {
			return fmt.Sprintf("$%.1fK", f/1000)
		}
		return fmt.Sprintf("$%.0f", f)
	},
	"score":      func(f float64) string { return fmt.Sprintf("%.3f", f) },
	"scoreWidth": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"confClass": func(c string) string {
		switch c {
		case "MATCH":
			return "conf-match"
		case "PROBABLE_MATCH":
			return "conf-probable"
		default:
			return "conf-no"
		}
	},
	"cardClass": func(c string) string {
		switch c {
		case "MATCH":
			return "card-match"
		case "PROBABLE_MATCH":
			return "card-probable"
		default:
			return ""
		}
	},
	"confIcon": func(c string) string {
		switch c {
		case "MATCH":
			return "check_circle"
		case "PROBABLE_MATCH":
			return "help"
		default:
			return "cancel"
		}
	},
	"venueClass": func(v string) string {
		if v == "polymarket" {
			return "venue-poly"
		}
		return "venue-kalshi"
	},
	"venueIcon": func(v string) string {
		if v == "polymarket" {
			return "P"
		}
		return "K"
	},
	"bigVol": func(f float64) bool { return f >= 1000 },
	"inc":    func(i int) int { return i + 1 },
}).Parse(pageHTML))
