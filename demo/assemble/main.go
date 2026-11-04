// Command assemble builds the static site for linkly's in-browser demo
// (see cmd/linkly-wasm). It copies the real web UI from internal/httpapi/web
// unchanged, adapts its index.html for hosting under a sub-path such as
// https://user.github.io/linkly/ (relative asset paths, a CSP that allows
// WebAssembly, the demo banner and glue script), and adds the demo files.
//
// linkly.wasm and wasm_exec.js must already be in the output directory
// (the Makefile's demo target puts them there) so they can be versioned
// with a content hash for cache busting.
//
// Every edit to index.html must match exactly once; if the UI changes so
// an edit no longer applies, the build fails instead of shipping a
// half-adapted page.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// demoCSP differs from the server's UI policy (internal/httpapi/web.go)
// only where the demo needs it: 'wasm-unsafe-eval' to compile the module
// and blob: images for QR codes rendered by Go. frame-ancestors cannot be
// set from a meta tag.
const demoCSP = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; " +
	"img-src 'self' blob:; connect-src 'self'; base-uri 'none'; form-action 'self'"

const repoURL = "https://github.com/SAMARTHJAISWAL/linkly"

func main() {
	webDir := flag.String("web", "internal/httpapi/web", "the real UI's assets")
	demoDir := flag.String("demo", "demo/web", "demo-only assets")
	out := flag.String("out", "dist-demo", "output directory (must contain linkly.wasm and wasm_exec.js)")
	flag.Parse()
	if err := run(*webDir, *demoDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "assemble:", err)
		os.Exit(1)
	}
}

func run(webDir, demoDir, out string) error {
	if err := os.MkdirAll(filepath.Join(out, "static"), 0o755); err != nil {
		return err
	}

	// The UI's assets, byte for byte.
	entries, err := os.ReadDir(webDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "index.html" {
			continue
		}
		if err := copyFile(filepath.Join(webDir, e.Name()), filepath.Join(out, "static", e.Name())); err != nil {
			return err
		}
	}
	for _, name := range []string{"demo.js", "demo.css"} {
		if err := copyFile(filepath.Join(demoDir, name), filepath.Join(out, name)); err != nil {
			return err
		}
	}

	// Content hashes for cache busting: Pages caches files for minutes, and
	// a new wasm_exec.js must never meet an old linkly.wasm or vice versa.
	ver := map[string]string{}
	for _, name := range []string{"linkly.wasm", "wasm_exec.js", "demo.js", "demo.css", "static/app.js", "static/style.css"} {
		h, err := hashFile(filepath.Join(out, name))
		if err != nil {
			return fmt.Errorf("%w (run `make demo`, which builds the wasm first)", err)
		}
		ver[name] = name + "?v=" + h
	}

	index, err := os.ReadFile(filepath.Join(webDir, "index.html"))
	if err != nil {
		return err
	}
	page, err := adapt(string(index), ver)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte(page), 0o644); err != nil {
		return err
	}
	// Serve files as-is on GitHub Pages (no Jekyll processing).
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

// adapt rewrites the server's index.html for the static demo.
func adapt(page string, ver map[string]string) (string, error) {
	edits := []struct{ old, new string }{
		{`<meta charset="utf-8">`, `<meta charset="utf-8">` + "\n  " +
			`<meta http-equiv="Content-Security-Policy" content="` + demoCSP + `">` + "\n  " +
			`<meta name="referrer" content="strict-origin-when-cross-origin">`},
		{`<title>linkly</title>`, `<title>linkly · live demo</title>`},
		{`<link rel="icon" href="/static/favicon.svg"`, `<link rel="icon" href="static/favicon.svg"`},
		{`<link rel="stylesheet" href="/static/style.css">`,
			`<link rel="stylesheet" href="` + ver["static/style.css"] + `">` + "\n  " +
				`<link rel="stylesheet" href="` + ver["demo.css"] + `">`},
		// Order matters and defer preserves it: the Go runtime support, then
		// the demo glue (which shims fetch), then the unmodified UI.
		{`<script src="/static/app.js" defer></script>`,
			`<script src="` + ver["wasm_exec.js"] + `" defer></script>` + "\n  " +
				`<script src="` + ver["demo.js"] + `" data-wasm="` + ver["linkly.wasm"] + `" defer></script>` + "\n  " +
				`<script src="` + ver["static/app.js"] + `" defer></script>`},
		{`<a class="brand" href="/"`, `<a class="brand" href="#/"`},
		{`<img src="/static/favicon.svg"`, `<img src="static/favicon.svg"`},
		{`<a href="/metrics">metrics</a> · <a href="/healthz">health</a>`,
			`<a href="#/" data-demo-get="/metrics">metrics</a> · <a href="#/" data-demo-get="/healthz">health</a> · ` +
				`<a href="` + repoURL + `" rel="noopener">source</a>`},
	}
	for _, e := range edits {
		if n := strings.Count(page, e.old); n != 1 {
			return "", fmt.Errorf("index.html: expected exactly one %q, found %d; update demo/assemble", e.old, n)
		}
		page = strings.Replace(page, e.old, e.new, 1)
	}
	if strings.Contains(page, `"/static/`) {
		return "", fmt.Errorf("index.html: an absolute /static/ reference is left; update demo/assemble")
	}
	return page, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:5]), nil
}
