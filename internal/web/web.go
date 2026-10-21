// Package web is the public face: one page with the logo, and a 404 for
// every other address. The gateway's real endpoints sit behind a secret path
// and are mounted here by the caller.
package web

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"html/template"
	"net/http"
	"strings"
)

//go:embed logo.svg
var logoSVG []byte

var page = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Fyutt</title>
<link rel="icon" href="data:image/svg+xml;base64,{{.Icon}}">
<style>
html,body{margin:0;height:100%;background:#fff}
body{display:flex;align-items:center;justify-content:center}
svg{width:min(50vw,240px);height:auto}
</style>
</head>
<body>
{{.Logo}}
</body>
</html>
`))

var indexHTML = func() []byte {
	// The file starts with an XML prolog and a DOCTYPE, which have no place
	// inside an HTML document: keep only the <svg> element itself.
	svg := logoSVG[bytes.Index(logoSVG, []byte("<svg")):]
	svg = bytes.Replace(svg, []byte("<svg "), []byte(`<svg role="img" aria-label="Fyutt" `), 1)
	var b bytes.Buffer
	if err := page.Execute(&b, map[string]any{
		"Logo": template.HTML(svg),
		"Icon": base64.StdEncoding.EncodeToString(logoSVG),
	}); err != nil {
		panic(err)
	}
	return b.Bytes()
}()

// Router sends "/" to the logo, "/<gate>/..." to gate, and everything else to
// a 404 — the same 404 a rejected gateway request gets.
type Router struct {
	GatePath string
	Gate     http.Handler
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")

	if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
		h.Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(indexHTML)
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/"+rt.GatePath+"/"); ok && rt.GatePath != "" {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		r2.URL.RawPath = ""
		rt.Gate.ServeHTTP(w, r2)
		return
	}
	NotFound(w)
}

// NotFound is the one 404 the whole site gives. Bodies and headers do not
// differ between "no such page" and "wrong key", so neither tells a scanner
// anything.
func NotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("Not Found\n"))
}
