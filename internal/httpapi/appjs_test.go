package httpapi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// appJSHarness loads the UI script into a Node vm context with a minimal
// fake DOM, sets location.hash, runs it (which routes once) and prints what
// the page would show as JSON.
const appJSHarness = `
const fs = require("fs"), vm = require("vm");
const [src, hash] = process.argv.slice(2);
const els = {};
function el(id) {
  return els[id] ??= {
    id, hidden: true, textContent: "", value: "", dataset: {}, style: {},
    addEventListener() {}, focus() {}, setAttribute() {}, removeAttribute() {},
    replaceChildren() {}, appendChild() {}, append() {},
  };
}
const fetched = [];
const ctx = {
  document: { getElementById: el, querySelectorAll: () => [], createElement: () => el("_new") },
  window: { addEventListener() {} },
  location: { hash },
  fetch: (url) => { fetched.push(String(url)); return new Promise(() => {}); },
  Intl, URL, Date, Math, Number, setTimeout, console,
};
let error = null;
try {
  vm.runInNewContext(fs.readFileSync(src, "utf8"), ctx, { filename: "app.js" });
} catch (e) {
  error = String(e);
}
const err = els["stats-error"] || {};
console.log(JSON.stringify({ error, statsError: err.textContent || "", statsErrorShown: err.hidden === false, fetched }));
`

type appJSResult struct {
	Error           *string  `json:"error"`
	StatsError      string   `json:"statsError"`
	StatsErrorShown bool     `json:"statsErrorShown"`
	Fetched         []string `json:"fetched"`
}

func runAppJS(t *testing.T, hash string) appJSResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	harness := filepath.Join(t.TempDir(), "harness.js")
	if err := os.WriteFile(harness, []byte(appJSHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness, "web/app.js", hash).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var res appJSResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	return res
}

// TestAppJSMalformedStatsHash: a malformed percent-escape in the stats
// route made decodeURIComponent throw, so the UI script died on load.
func TestAppJSMalformedStatsHash(t *testing.T) {
	for _, hash := range []string{"#/stats/%E0", "#/stats/%", "#/stats/abc%zz"} {
		res := runAppJS(t, hash)
		if res.Error != nil {
			t.Fatalf("%s: script threw: %s", hash, *res.Error)
		}
		if !res.StatsErrorShown || res.StatsError == "" {
			t.Fatalf("%s: no error shown: %+v", hash, res)
		}
		if len(res.Fetched) != 0 {
			t.Fatalf("%s: fetched %v for an invalid code", hash, res.Fetched)
		}
	}

	res := runAppJS(t, "#/stats/my%2Dcode")
	if res.Error != nil || len(res.Fetched) != 1 || res.Fetched[0] != "/api/links/my-code" {
		t.Fatalf("valid escaped code: %+v", res)
	}
}
