package main

import (
	"os"
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
