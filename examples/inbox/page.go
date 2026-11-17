package main

import (
	_ "embed"
	"net/http"
)

// pageHTML is a minimal visitor chat on the widget protocol, for trying the
// host in a browser.
//
//go:embed page.html
var pageHTML []byte

func servePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	_, _ = w.Write(pageHTML)
}
