// Live activity page: renders the state pushed by /live/events (Server-Sent Events).
// All text from the server goes through textContent; links are only made for http(s) URLs.
"use strict";

const $ = (id) => document.getElementById(id);
let offset = 0;          // server clock minus local clock (ms)
let state = null;
const feed = [];
const MAX_FEED = 200;
const ICONS = { info: "ℹ️", change: "🆕", release: "📦", prune: "🧹", error: "❌", gone: "🪦", recovered: "♻️" };

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

function display(url) {
  return String(url || "").replace(/^[a-z+]+:\/\//i, "").replace(/\/$/, "");
}

function repoLink(url, cls) {
  const ok = /^https?:\/\//i.test(url || "");
  const a = el(ok ? "a" : "span", cls, display(url));
  if (ok) { a.href = url; a.target = "_blank"; a.rel = "noopener noreferrer"; }
  return a;
}

function now() { return Date.now() + offset; }

function dur(ms) {
  if (!(ms >= 0)) return "";
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m " + (s % 60) + "s";
  const h = Math.floor(m / 60);
  return h + "h " + (m % 60) + "m";
}

function size(b) {
  if (!(b > 0)) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i ? b.toFixed(1) : b) + " " + u[i];
}

function amount(n, unit) { return unit === "bytes" ? size(n) : Number(n).toLocaleString(); }

function since(t) {
  const v = Date.parse(t);
  return v > 0 ? now() - v : -1;
}

function renderCycle() {
  const c = state.cycle;
  const bar = $("cyclebar").parentElement;
  if (c.running) {
    $("cycle").textContent = `Checking repositories: ${c.done} of ${c.total} done · running for ${dur(since(c.started))}`;
    $("cyclebar").style.width = (c.total ? (100 * c.done) / c.total : 0) + "%";
    bar.hidden = false;
  } else {
    let s = "Idle";
    const next = Date.parse(c.next);
    if (next > 0) s += next > now() ? ` · next check cycle in ${dur(next - now())}` : " · next check cycle starting";
    if (Date.parse(c.last_end) > 0) s += ` · last cycle took ${dur(c.last_took_ms)}, ${dur(since(c.last_end))} ago`;
    $("cycle").textContent = s;
    bar.hidden = true;
  }
  $("disk").textContent = state.disk_total ? `Storage: ${size(state.disk_free)} free of ${size(state.disk_total)}` : "";
}

function renderSlots() {
  const box = $("slots");
  box.replaceChildren();
  for (const s of state.slots) {
    const busy = Date.parse(s.started) > 0;
    const card = el("article", "slot" + (busy ? "" : " idle") + (s.worker ? "" : " temp"));
    const top = el("div", "top");
    top.append(el("span", "", s.label), el("span", "elapsed", busy ? dur(since(s.started)) : ""));
    card.append(top);
    if (!busy) {
      card.append(el("div", "step muted", "Idle"));
      box.append(card);
      continue;
    }
    card.append(repoLink(s.repo, "repo"));
    if (s.about) card.append(el("div", "about", s.about));
    card.append(el("div", "step", s.step || ""));
    if (s.detail) card.append(el("div", "detail", s.detail));
    const bar = el("div", "bar");
    const fill = el("div", "fill");
    bar.append(fill);
    if (s.total > 0) {
      fill.style.width = Math.min(100, (100 * s.done) / s.total) + "%";
    } else {
      bar.classList.add("indeterminate");
    }
    card.append(bar);
    if (s.unit) {
      const nums = el("div", "nums");
      let left = amount(s.done, s.unit) + (s.total > 0 ? " of " + amount(s.total, s.unit) : "");
      let right = "";
      if (s.rate > 0) {
        right = (s.unit === "bytes" ? size(s.rate) + "/s" : Math.round(s.rate).toLocaleString() + "/s");
        if (s.total > s.done) right += " · " + dur((1000 * (s.total - s.done)) / s.rate) + " left";
      }
      nums.append(el("span", "", left), el("span", "", right));
      card.append(nums);
    }
    box.append(card);
  }
}

function renderFeed() {
  const list = $("feed");
  list.replaceChildren();
  for (let i = feed.length - 1; i >= 0; i--) {
    const e = feed[i];
    const li = el("li", e.kind);
    const t = new Date(Date.parse(e.time));
    li.append(el("span", "t", t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })));
    li.append(el("span", "icon", ICONS[e.kind] || "•"));
    const msg = el("span", "msg");
    if (e.repo) {
      msg.append(repoLink(e.repo), document.createTextNode(" — "));
    }
    msg.append(document.createTextNode(e.text));
    li.append(msg);
    list.append(li);
  }
  $("nofeed").hidden = feed.length > 0;
}

let lastSeq = 0;
function onState(data) {
  state = JSON.parse(data);
  offset = Date.parse(state.now) - Date.now();
  let added = false;
  for (const e of state.events) {
    if (e.seq > lastSeq) { feed.push(e); lastSeq = e.seq; added = true; }
  }
  if (state.last_seq < lastSeq) {         // the bot restarted: start over
    feed.length = 0; lastSeq = 0;
    for (const e of state.events) { feed.push(e); lastSeq = Math.max(lastSeq, e.seq); }
    added = true;
  }
  while (feed.length > MAX_FEED) feed.shift();
  renderCycle();
  renderSlots();
  if (added) renderFeed();
}

function setConn(on) {
  const c = $("conn");
  c.className = "conn " + (on ? "on" : "off");
  c.textContent = on ? "live" : "reconnecting";
  c.title = on ? "Receiving live updates" : "Connection lost; reconnecting…";
}

function connect() {
  const es = new EventSource("/live/events");
  es.addEventListener("state", (ev) => onState(ev.data));
  es.onopen = () => setConn(true);
  es.onerror = () => setConn(false); // EventSource reconnects by itself
}

// Elapsed times keep ticking between updates.
setInterval(() => { if (state) { renderCycle(); renderSlots(); } }, 1000);
connect();
