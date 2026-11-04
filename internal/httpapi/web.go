package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webFS embed.FS

// uiCSP locks the UI down to its own origin: no inline scripts or styles,
// no third-party requests, and no framing.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func webRoot() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embed directive guarantees the directory exists
	}
	return sub
}

// handleIndex serves the single-page UI at "/".
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	setUIHeaders(w)
	http.ServeFileFS(w, r, webRoot(), "index.html")
}

// staticHandler serves the UI's assets under /static/.
func staticHandler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(webRoot()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setUIHeaders(w)
		// Assets are not fingerprinted, so revalidate rather than cache blindly.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func setUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
}
