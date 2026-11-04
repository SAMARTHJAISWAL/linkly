package main

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAdaptRealIndex fails when the UI's index.html changes in a way the
// demo's edits no longer match, so the Pages build cannot silently break.
func TestAdaptRealIndex(t *testing.T) {
	index, err := os.ReadFile("../../internal/httpapi/web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ver := map[string]string{}
	for _, name := range []string{"linkly.wasm", "wasm_exec.js", "demo.js", "demo.css", "static/app.js", "static/style.css"} {
		ver[name] = name + "?v=test"
	}
	page, err := adapt(string(index), ver)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"'wasm-unsafe-eval'",
		`<script src="wasm_exec.js?v=test" defer></script>`,
		`data-wasm="linkly.wasm?v=test"`,
		`data-demo-get="/metrics"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("adapted page lacks %s", want)
		}
	}
	wasmExec := strings.Index(page, "wasm_exec.js")
	demo := strings.Index(page, `src="demo.js`)
	app := strings.Index(page, "static/app.js")
	if !(wasmExec < demo && demo < app) {
		t.Errorf("scripts out of order: wasm_exec %d, demo %d, app %d", wasmExec, demo, app)
	}
}

// TestRunWritesSite runs the whole assembler against the real UI and
// checks the Pages fallback page is part of the output.
func TestRunWritesSite(t *testing.T) {
	out := t.TempDir()
	for _, name := range []string{"linkly.wasm", "wasm_exec.js"} {
		if err := os.WriteFile(filepath.Join(out, name), []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run("../../internal/httpapi/web", "../web", out, "/linkly/"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "404.html", ".nojekyll", "demo.js", "static/app.js"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func TestNotFoundPageRejectsUnsafeBase(t *testing.T) {
	for _, base := range []string{"", "linkly/", "/linkly", "//evil.example/", "/a b/", "/x/../", `/"/`, "/a//b/"} {
		if _, err := notFoundPage(base); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	for _, base := range []string{"/", "/linkly/", "/a/b-c_d.e/"} {
		if _, err := notFoundPage(base); err != nil {
			t.Errorf("base %q: %v", base, err)
		}
	}
}

// TestNotFoundPageForwarding runs 404.html's script under Node with a fake
// location for each path and checks where it sends the browser: only
// code-shaped paths directly under the base are forwarded, and always to a
// same-origin path, never a protocol-relative or absolute URL.
func TestNotFoundPageForwarding(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page, err := notFoundPage("/linkly/")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no inline script in 404.html")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(page, hash) {
		t.Fatalf("CSP does not allow the inline script by hash %s", hash)
	}

	tests := map[string]string{ // path -> replace() target, "" for none
		"/linkly/aZ3kQ9x":                    "/linkly/#/aZ3kQ9x",
		"/linkly/my-alias_2/":                "/linkly/#/my-alias_2",
		"/linkly/" + strings.Repeat("a", 32): "/linkly/#/" + strings.Repeat("a", 32),
		"/linkly/" + strings.Repeat("a", 33): "",
		"/linkly/":                           "",
		"/linkly/a/b":                        "",
		"/linkly/static/x.js":                "",
		"/linkly/a.b":                        "",
		"/linkly/%E0":                        "",
		"/linkly/javascript:alert(1)":        "",
		"/other/abc":                         "",
		"//evil.example/abc":                 "",
		"/linkly//evil.example":              "",
		"/linkly/\\evil.example":             "",
	}
	script := filepath.Join(t.TempDir(), "s.js")
	os.WriteFile(script, []byte(m[1]), 0o644)
	harness := filepath.Join(t.TempDir(), "h.js")
	os.WriteFile(harness, []byte(`
const fs = require("fs"), vm = require("vm");
const [src, pathname] = process.argv.slice(2);
let target = "";
vm.runInNewContext(fs.readFileSync(src, "utf8"), { location: { pathname, replace: (u) => { target = String(u); } } });
process.stdout.write(target);
`), 0o644)
	for path, want := range tests {
		out, err := exec.Command(node, harness, script, path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", path, err, out)
		}
		if string(out) != want {
			t.Errorf("%s -> %q, want %q", path, out, want)
		}
	}
}
