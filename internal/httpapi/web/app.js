// linkly web UI. No framework, no build step. All user-controlled strings
// are inserted with textContent, never innerHTML.
"use strict";

const $ = (id) => document.getElementById(id);

function show(el, visible) { el.hidden = !visible; }

function setError(el, msg) {
  el.textContent = msg || "";
  show(el, Boolean(msg));
}

async function api(path, options) {
  const res = await fetch(path, options);
  let body = null;
  if ((res.headers.get("Content-Type") || "").includes("application/json")) {
    body = await res.json();
  }
  if (!res.ok) {
    let msg = (body && body.error) || `Request failed (${res.status})`;
    if (res.status === 429) {
      const after = res.headers.get("Retry-After");
      msg = `Too many links created. Try again in ${after || "a few"} seconds.`;
    }
    throw new Error(msg);
  }
  return body;
}

// ---------- routing ----------

function route() {
  const hash = location.hash || "#/";
  const stats = hash.startsWith("#/stats");
  show($("view-shorten"), !stats);
  show($("view-stats"), stats);
  document.querySelectorAll("[data-nav]").forEach((a) => {
    const active = (a.dataset.nav === "stats") === stats;
    if (active) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  if (stats) {
    const raw = hash.slice("#/stats/".length);
    let code;
    try {
      code = decodeURIComponent(raw);
    } catch {
      // A malformed escape such as "%E0": say so instead of throwing.
      show($("stats"), false);
      setError($("stats-error"), `Invalid code “${raw}” in the address.`);
      return;
    }
    if (hash.startsWith("#/stats/") && code) {
      $("stats-code").value = code;
      loadStats(code);
    } else {
      show($("stats"), false);
      $("stats-code").focus();
    }
  }
}

window.addEventListener("hashchange", route);

// ---------- create ----------

$("create-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const err = $("create-error");
  setError(err, "");

  const url = $("url").value.trim();
  if (!url) { setError(err, "Enter a URL to shorten."); $("url").focus(); return; }

  const payload = { url };
  const alias = $("alias").value.trim();
  if (alias) payload.alias = alias;
  const secs = Number($("expiry").value);
  if (secs > 0) payload.expires_at = new Date(Date.now() + secs * 1000).toISOString();

  const btn = $("create-btn");
  btn.disabled = true;
  btn.textContent = "Shortening…";
  try {
    const link = await api("/api/links", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    renderResult(link);
  } catch (e) {
    setError(err, e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Shorten";
  }
});

function renderResult(link) {
  const a = $("short-url");
  a.textContent = link.short_url;
  a.href = link.short_url;
  $("target-url").textContent = link.url;
  const note = $("expiry-note");
  if (link.expires_at) {
    note.textContent = `Expires ${new Date(link.expires_at).toLocaleString()}`;
    show(note, true);
  } else {
    show(note, false);
  }
  $("qr").src = `/api/links/${encodeURIComponent(link.code)}/qr.png?size=320`;
  $("stats-link").href = `#/stats/${encodeURIComponent(link.code)}`;
  $("copy-btn").textContent = "Copy";
  show($("result"), true);
}

$("copy-btn").addEventListener("click", async () => {
  const btn = $("copy-btn");
  const text = $("short-url").textContent;
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = "Copied ✓";
  } catch {
    // Clipboard API needs a secure context; fall back to selecting the text.
    const range = document.createRange();
    range.selectNodeContents($("short-url"));
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    btn.textContent = "Press ⌘/Ctrl+C";
  }
  setTimeout(() => { btn.textContent = "Copy"; }, 2000);
});

// ---------- stats ----------

$("stats-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  let code = $("stats-code").value.trim();
  // Accept a full short URL as well as a bare code.
  try { code = new URL(code).pathname.replace(/^\/+/, ""); } catch { /* bare code */ }
  if (code) location.hash = `#/stats/${encodeURIComponent(code)}`;
});

async function loadStats(code) {
  const err = $("stats-error");
  setError(err, "");
  try {
    const s = await api(`/api/links/${encodeURIComponent(code)}`);
    renderStats(s);
  } catch (e) {
    show($("stats"), false);
    setError(err, e.message === "link not found" ? `No link with code “${code}”.` : e.message);
  }
}

const fmtNum = new Intl.NumberFormat();
const fmtDay = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", timeZone: "UTC" });

function dayLabel(iso) { return fmtDay.format(new Date(iso + "T00:00:00Z")); }

function renderStats(s) {
  const a = $("s-short");
  a.textContent = s.short_url;
  a.href = s.short_url;
  $("s-target").textContent = s.url;
  let meta = `Created ${new Date(s.created_at).toLocaleString()}`;
  if (s.expires_at) meta += s.expired ? " · expired" : ` · expires ${new Date(s.expires_at).toLocaleString()}`;
  $("s-meta").textContent = meta;
  $("s-total").textContent = fmtNum.format(s.total_clicks);

  renderChart(s.clicks_by_day);
  renderReferrers(s.top_referrers);
  show($("stats"), true);
}

function renderChart(days) {
  const chart = $("chart");
  const tip = $("tooltip");
  chart.replaceChildren();
  const max = Math.max(1, ...days.map((d) => d.clicks));
  const sum = days.reduce((n, d) => n + d.clicks, 0);
  chart.setAttribute("aria-label", `Clicks per day over the last ${days.length} days: ${sum} in total, at most ${max} on one day.`);

  for (const d of days) {
    const hit = document.createElement("div");
    hit.className = "bar-hit";
    hit.tabIndex = 0;
    const label = `${dayLabel(d.date)}: ${fmtNum.format(d.clicks)} click${d.clicks === 1 ? "" : "s"}`;
    hit.setAttribute("aria-label", label);
    const bar = document.createElement("div");
    bar.className = "bar" + (d.clicks === 0 ? " zero" : "");
    bar.style.height = d.clicks === 0 ? "0" : `max(3px, ${(d.clicks / max) * 100}%)`;
    hit.appendChild(bar);

    const showTip = () => {
      const r = hit.getBoundingClientRect();
      tip.textContent = label;
      tip.style.left = `${r.left + r.width / 2}px`;
      tip.style.top = `${Math.min(r.bottom - 4, Math.max(bar.getBoundingClientRect().top, r.top + 20))}px`;
      show(tip, true);
    };
    const hideTip = () => show(tip, false);
    hit.addEventListener("mouseenter", showTip);
    hit.addEventListener("focus", showTip);
    hit.addEventListener("mouseleave", hideTip);
    hit.addEventListener("blur", hideTip);
    chart.appendChild(hit);
  }
  $("axis-start").textContent = days.length ? dayLabel(days[0].date) : "";
  $("axis-end").textContent = days.length ? dayLabel(days[days.length - 1].date) : "";

  const tbody = $("daily-table");
  tbody.replaceChildren();
  for (const d of [...days].reverse()) {
    const tr = document.createElement("tr");
    const td1 = document.createElement("td");
    td1.textContent = d.date;
    const td2 = document.createElement("td");
    td2.textContent = fmtNum.format(d.clicks);
    tr.append(td1, td2);
    tbody.appendChild(tr);
  }
}

function renderReferrers(refs) {
  const list = $("referrers");
  list.replaceChildren();
  show($("no-referrers"), refs.length === 0);
  const max = Math.max(1, ...refs.map((r) => r.clicks));
  for (const r of refs) {
    const li = document.createElement("li");
    const host = document.createElement("span");
    host.className = "host";
    host.textContent = r.host === "(direct)" ? "Direct / unknown" : r.host;
    const count = document.createElement("span");
    count.className = "count";
    count.textContent = fmtNum.format(r.clicks);
    const meter = document.createElement("div");
    meter.className = "meter";
    const fill = document.createElement("span");
    fill.style.width = `${(r.clicks / max) * 100}%`;
    meter.appendChild(fill);
    li.append(host, count, meter);
    list.appendChild(li);
  }
}

route();
