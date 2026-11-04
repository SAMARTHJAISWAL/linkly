// linkly live demo glue. Loaded before the real UI script (static/app.js),
// it starts linkly's Go code compiled to WebAssembly and routes the UI's
// same-origin requests into it instead of the network:
//
//   - fetch("/api/...") calls go through linkly.handle (the real Go
//     http.Handler) and come back as ordinary Response objects;
//   - <img src="/api/.../qr.png"> is resolved the same way into a blob URL;
//   - short links are hash routes (#/<code>): opening one runs GET /<code>
//     through Go, waits for the click to be persisted, then follows the 302.
//
// Nothing here talks to a server; state lives in this browser only.
"use strict";

(() => {
  const script = document.currentScript;
  const wasmURL = (script && script.dataset.wasm) || "linkly.wasm";
  const REPO = "https://github.com/SAMARTHJAISWAL/linkly";
  const CODE_ROUTE = /^#\/([A-Za-z0-9_-]{1,32})$/;

  // Short links are <page URL>#/<code>; the Go side builds them from this.
  const baseURL = location.origin + location.pathname + "#";

  // ---------- boot the Go runtime ----------

  let markReady, markFailed;
  const ready = new Promise((res, rej) => { markReady = res; markFailed = rej; });
  ready.catch(() => {}); // failures are reported in the banner

  window.linklyConfig = {
    baseURL,
    storageKey: "linkly-demo:v1",
    onReady: () => markReady(window.linkly),
  };

  async function boot() {
    const go = new Go();
    let instance;
    try {
      ({ instance } = await WebAssembly.instantiateStreaming(fetch(wasmURL), go.importObject));
    } catch (e) {
      // Some static hosts serve .wasm without the application/wasm type
      // that streaming compilation insists on.
      const res = await fetch(wasmURL);
      if (!res.ok) throw new Error(`could not load ${wasmURL} (HTTP ${res.status})`);
      ({ instance } = await WebAssembly.instantiate(await res.arrayBuffer(), go.importObject));
    }
    go.run(instance).then(() => markFailed(new Error("the Go program exited")));
  }

  // ---------- requests into Go ----------

  function isLocalAPI(url) {
    return url.origin === location.origin && (url.pathname.startsWith("/api/") ||
      url.pathname === "/metrics" || url.pathname === "/healthz");
  }

  async function goRequest(method, pathAndQuery, headers, body) {
    const linkly = await ready;
    return linkly.handle(method, pathAndQuery, headers || {}, body || null);
  }

  const NULL_BODY = new Set([101, 103, 204, 205, 304]);

  const realFetch = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const req = new Request(input, init);
    const url = new URL(req.url);
    if (!isLocalAPI(url)) return realFetch(input, init);
    const body = ["GET", "HEAD"].includes(req.method) ? null : new Uint8Array(await req.arrayBuffer());
    const res = await goRequest(req.method, url.pathname + url.search, Object.fromEntries(req.headers), body);
    return new Response(NULL_BODY.has(res.status) ? null : res.body, { status: res.status, headers: res.headers });
  };

  // Images cannot be intercepted with fetch, so the src setter of each <img>
  // is wrapped: API URLs are rendered by Go and shown as blob: URLs.
  const srcProp = Object.getOwnPropertyDescriptor(HTMLImageElement.prototype, "src");
  function routeImage(img) {
    let blobURL = "";
    Object.defineProperty(img, "src", {
      configurable: true,
      get() { return srcProp.get.call(this); },
      set(value) {
        const url = new URL(value, location.href);
        if (!isLocalAPI(url)) { srcProp.set.call(this, value); return; }
        goRequest("GET", url.pathname + url.search).then((res) => {
          if (res.status !== 200) throw new Error(`HTTP ${res.status}`);
          if (blobURL) URL.revokeObjectURL(blobURL);
          blobURL = URL.createObjectURL(new Blob([res.body], { type: res.headers["Content-Type"] || "image/png" }));
          srcProp.set.call(this, blobURL);
        }).catch((e) => console.warn("linkly demo: image", url.pathname, e.message));
      },
    });
  }
  document.querySelectorAll("img").forEach(routeImage);

  // ---------- banner, overlay, dialogs ----------

  const $ = (id) => document.getElementById(id);

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, v);
    node.append(...children);
    return node;
  }

  const status = el("span", { id: "demo-status", class: "demo-status" }, "Loading the Go runtime…");
  const banner = el("aside", { class: "demo-banner", role: "note" },
    el("strong", {}, "Live demo"),
    " — linkly's Go code compiled to WebAssembly and running in your browser. ",
    "Links are stored only in this browser (localStorage); the real server uses SQLite. ",
    el("a", { href: REPO, rel: "noopener" }, "Source on GitHub"),
    " ", status);
  document.body.prepend(banner);

  ready.then((linkly) => {
    const storage = linkly.storage();
    status.textContent = storage === "ok"
      ? `Running (${linkly.goVersion}).`
      : `Running (${linkly.goVersion}). Storage is unavailable here, so links last only until this tab closes.`;
  }, (e) => {
    status.textContent = `Could not start the WebAssembly module: ${e.message}`;
    status.classList.add("demo-failed");
  });

  const overlay = el("div", { class: "demo-overlay", hidden: "" });
  document.body.append(overlay);

  function showOverlay(title, detail, withBack) {
    const card = el("div", { class: "card demo-overlay-card" }, el("h1", {}, title));
    if (detail) card.append(el("p", { class: "muted" }, detail));
    if (withBack) card.append(el("a", { href: "#/", class: "text-link" }, "← Back to the demo"));
    overlay.replaceChildren(card);
    overlay.hidden = false;
  }

  // ---------- short-link redirects ----------

  async function follow(code, referer) {
    showOverlay("Opening short link…", `Resolving /${code} with linkly's redirect handler.`, false);
    try {
      const res = await goRequest("GET", "/" + code, referer ? { Referer: referer } : {});
      if (res.status === 302) {
        const target = res.headers.Location;
        const proto = new URL(target).protocol; // the service only stores http(s) URLs
        if (proto !== "http:" && proto !== "https:") throw new Error("refusing a non-http target");
        // The click sits in the recorder's queue; make sure it is written
        // to the store, and so to localStorage, before leaving the page.
        await (await ready).flush();
        location.replace(target);
        return;
      }
      const text = new TextDecoder().decode(res.body).trim();
      if (res.status === 404) {
        showOverlay(`404 · No link “${code}” here`,
          "Demo links exist only in the browser that created them (they are kept in its localStorage).", true);
      } else if (res.status === 410) {
        showOverlay("410 · This short link has expired", `“${code}” passed its expiry time, so linkly answers 410 Gone.`, true);
      } else {
        showOverlay(`${res.status} · ${text || "Unexpected response"}`, "", true);
      }
    } catch (e) {
      showOverlay("Could not open the short link", e.message, true);
    }
  }

  function codeFromHash(hash) {
    const m = CODE_ROUTE.exec(hash);
    // "stats" is a reserved alias, so #/stats is always the UI's own route.
    return m && m[1].toLowerCase() !== "stats" ? m[1] : "";
  }

  window.addEventListener("hashchange", () => {
    const code = codeFromHash(location.hash);
    if (code) follow(code, location.origin + location.pathname);
    else overlay.hidden = true;
  });

  // ---------- small adaptations of the real UI ----------

  // Accept a full demo short link (…/#/code) in the stats lookup. Runs in
  // the capture phase, before the UI's own submit handler reads the field.
  document.addEventListener("submit", (ev) => {
    const input = $("stats-code");
    if (!input || ev.target !== $("stats-form")) return;
    try {
      const code = codeFromHash(new URL(input.value.trim()).hash);
      if (code) input.value = code;
    } catch { /* a bare code */ }
  }, true);

  const statsInput = $("stats-code");
  if (statsInput) statsInput.placeholder = `aZ3kQ9x or ${baseURL}/aZ3kQ9x`;

  // Footer links to /metrics and /healthz: show the Go handler's output.
  const dialog = el("dialog", { class: "demo-dialog" });
  const dialogTitle = el("h2", {});
  const dialogBody = el("pre", {});
  const closeBtn = el("button", { type: "button", class: "secondary" }, "Close");
  closeBtn.addEventListener("click", () => dialog.close());
  dialog.append(dialogTitle, dialogBody, closeBtn);
  document.body.append(dialog);

  document.querySelectorAll("[data-demo-get]").forEach((a) => {
    a.addEventListener("click", async (ev) => {
      ev.preventDefault();
      const path = a.dataset.demoGet;
      const res = await goRequest("GET", path);
      dialogTitle.textContent = `GET ${path} → ${res.status}`;
      dialogBody.textContent = new TextDecoder().decode(res.body);
      dialog.showModal();
    });
  });

  // ---------- go ----------

  const initial = codeFromHash(location.hash);
  if (initial) follow(initial, document.referrer);

  boot().catch((e) => markFailed(e));
})();
