// SPDX-License-Identifier: AGPL-3.0-or-later
"use strict";

// The server puts a per-process token into the page; every POST sends it back.
const token = document.querySelector('meta[name="skra-token"]').content;

// post sends body as JSON, or as it is when it is a FormData, with the token.
async function post(url, body) {
  const headers = { "X-Skra-Token": token };
  let payload = body;
  if (!(body instanceof FormData)) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body ?? {});
  }
  return fetch(url, { method: "POST", headers, body: payload });
}

// --- live updates ---
// One EventSource per visible page. The server sends the whole overview on
// connect and then only what changed, as HTML fragments that each replace the
// element with the same id as the event.
let source = null;
const slots = ["printers", "queue", "recent", "need", "foot"];

function swap(id, html) {
  const el = document.getElementById(id);
  if (el) el.outerHTML = html;
}

function connect() {
  if (source) return;
  source = new EventSource("/events");
  for (const id of slots) {
    source.addEventListener(id, (e) => swap(id, e.data));
  }
  // The browser reconnects on its own, and the server then sends everything
  // again; until it does, say so instead of showing stale numbers quietly.
  source.onerror = () => {
    const msg = document.getElementById("msg");
    if (msg) {
      msg.textContent = "Lost the connection to skrá. Reconnecting.";
      msg.classList.add("bad");
    }
  };
}

function disconnect() {
  if (source) source.close();
  source = null;
}

// --- clock ---
// A browser timer at minute resolution; the service itself never ticks.
let clockTimer = null;

function showClock() {
  const el = document.getElementById("clock");
  if (el) {
    el.textContent = new Date()
      .toLocaleString("en-GB", { weekday: "short", day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" })
      .replace(",", "");
  }
  clockTimer = setTimeout(showClock, 60000 - (Date.now() % 60000) + 50);
}

// --- visibility ---
// A hidden tab holds no connection, so the service stops polling for it.
function watch() {
  clearTimeout(clockTimer);
  if (document.hidden) {
    disconnect();
  } else {
    showClock();
    connect();
  }
}
document.addEventListener("visibilitychange", watch);
watch();
