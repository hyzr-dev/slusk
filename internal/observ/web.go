// Package observ: web.go embeds and serves the dashboard's static assets
// (HTML shell + JS). The template has no server-side data holes — all data
// is fetched client-side from /api/jobs so this package never needs to
// import html/template's data-binding surface beyond a single static parse.
package observ

import (
	"embed"
	"net/http"
)

//go:embed web/dashboard.html
var dashboardHTML embed.FS

//go:embed web/dashboard.js
var dashboardJS embed.FS

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	b, err := dashboardHTML.ReadFile("web/dashboard.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func dashboardJSHandler(w http.ResponseWriter, r *http.Request) {
	b, err := dashboardJS.ReadFile("web/dashboard.js")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(b)
}
