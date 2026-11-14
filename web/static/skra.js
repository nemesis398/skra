// SPDX-License-Identifier: AGPL-3.0-or-later
"use strict";

// The server puts a per-process token into the page; every POST sends it back.
const token = document.querySelector('meta[name="skra-token"]').content;

const $ = (id) => document.getElementById(id);

// post sends body as JSON, or as it is when it is a FormData or a
// URLSearchParams, with the token.
async function post(url, body) {
  const headers = { "X-Skra-Token": token };
  let payload = body;
  if (!(body instanceof FormData) && !(body instanceof URLSearchParams)) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body ?? {});
  }
  return fetch(url, { method: "POST", headers, body: payload });
}

// --- the footer message ---
// The footer shows what the server says about CUPS. An action's answer is shown
// over it for a few seconds; the footer is swapped on every poll, so the answer
// is put back after each swap until it expires.
let server = { text: "", bad: false };
let note = null;
let noteTimer = null;

function readServerMsg() {
  const el = $("msg");
  if (el) server = { text: el.textContent, bad: el.classList.contains("bad") };
  showMsg();
}

function showMsg() {
  const el = $("msg");
  if (!el) return;
  const m = note ?? server;
  el.textContent = m.text;
  el.classList.toggle("bad", m.bad);
}

function say(text, bad = false) {
  note = { text, bad };
  showMsg();
  clearTimeout(noteTimer);
  noteTimer = setTimeout(() => {
    note = null;
    showMsg();
  }, 8000);
}

// --- live updates ---
// One EventSource per visible page. The server sends the whole overview on
// connect and then only what changed, as HTML fragments that each replace the
// element with the same id as the event.
let source = null;
const slots = ["printers", "opts", "queue", "recent", "need", "foot"];

function swap(id, html) {
  const el = $(id);
  if (!el) return;
  el.outerHTML = html;
  if (id === "foot") readServerMsg();
  if (id === "opts") renderOpts();
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
    server = { text: "Lost the connection to skrá. Reconnecting.", bad: true };
    showMsg();
  };
}

function disconnect() {
  if (source) source.close();
  source = null;
}

// --- the confirm dialog ---
// One floating, framed dialog. It resolves false for the safe button or Escape
// and gives the focus back to where it was. For a plain confirm it resolves
// true; when choices ([[value, label], ...]) are given it resolves the picked
// value, with up to three as buttons and more as a select. restore is a
// selector for the button, because the poll may have swapped it for a new
// element while the dialog was open.
function confirmBox(title, text, yes, no, restore, choices, glyph = "\u{F0156}") {
  return new Promise((resolve) => {
    const d = $("dialog");
    const back = document.activeElement;
    $("dlg-title").textContent = title;
    $("dlg-text").textContent = text;
    const icon = document.createElement("i");
    icon.textContent = glyph;
    $("dlg-yes").replaceChildren(icon, yes);
    $("dlg-no").textContent = no;

    let picked = choices?.[0]?.[0];
    const box = $("dlg-choice");
    let first = $("dlg-no");
    box.replaceChildren();
    box.hidden = !choices;
    if (choices && choices.length > 3) {
      const select = document.createElement("select");
      select.setAttribute("aria-label", "Printer");
      for (const [v, label] of choices) select.append(new Option(label, v));
      select.onchange = () => (picked = select.value);
      box.append(select);
      first = select;
    } else if (choices) {
      const group = document.createElement("div");
      group.className = "seg";
      group.setAttribute("role", "group");
      const mark = () => {
        for (const b of group.children) {
          b.className = b.dataset.val === picked ? "on" : "";
          b.setAttribute("aria-pressed", String(b.dataset.val === picked));
        }
      };
      for (const [v, label] of choices) {
        const b = document.createElement("button");
        b.type = "button";
        b.dataset.val = v;
        b.textContent = label;
        b.onclick = () => {
          picked = v;
          mark();
        };
        group.append(b);
      }
      mark();
      box.append(group);
      first = group.children[0];
    }
    d.hidden = false;
    first.focus();
    const done = (v) => {
      d.hidden = true;
      d.onkeydown = $("dlg-yes").onclick = $("dlg-no").onclick = null;
      box.replaceChildren();
      box.hidden = true;
      const target = back?.isConnected ? back : restore && document.querySelector(restore);
      target?.focus?.();
      resolve(v && choices ? picked : v);
    };
    $("dlg-yes").onclick = () => done(true);
    $("dlg-no").onclick = () => done(false);
    d.onkeydown = (e) => {
      if (e.key === "Escape") done(false);
    };
  });
}

// --- actions ---
// Every action is a POST that answers with the sentence for the footer: the
// server's own words when it worked or when CUPS refused. The button that
// started it is off while the request runs and on again afterwards: the poll
// swaps a fragment only when its HTML changed, and a refused action changes
// nothing, so the swap cannot be what turns the button back on.
async function send(url, body, button) {
  if (button) button.disabled = true;
  try {
    const res = await post(url, body);
    say((await res.text()).trim() || (res.ok ? "Done." : `skrá answered ${res.status}.`), !res.ok);
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
  } finally {
    if (button?.isConnected) button.disabled = false;
  }
}

async function moveJob(b) {
  const { job } = b.dataset;
  let targets = [];
  try {
    targets = JSON.parse(b.dataset.targets || "[]");
  } catch (err) {
    targets = [];
  }
  if (targets.length === 0) return;
  const to = await confirmBox(b.dataset.moveTitle, `Choose the printer to move job ${job} to.`, b.dataset.moveLabel,
    "Keep it here", `[data-act="move"][data-job="${job}"]`, targets.map((t) => [t.name, t.label]), "\u{F0054}");
  if (!to) return;
  await send(`/jobs/${encodeURIComponent(job)}/move`, new URLSearchParams({ to }), b);
}

async function jobAction(b) {
  const { act, job } = b.dataset;
  if (act === "move") return moveJob(b);
  if (b.dataset.confirm) {
    const ok = await confirmBox(b.dataset.confirmTitle, b.dataset.confirm, b.dataset.confirmLabel, "Keep printing",
      `[data-act="${act}"][data-job="${job}"]`);
    if (!ok) return;
  }
  await send(`/jobs/${encodeURIComponent(job)}/${act === "reprint" ? "restart" : act}`, undefined, b);
}

async function testPage(b) {
  await send(`/printers/${encodeURIComponent(b.dataset.printer)}/testpage`, new URLSearchParams({ hold: String(opts.hold) }), b);
}

document.addEventListener("click", (e) => {
  const o = e.target.closest("[data-opt]");
  if (o) {
    const v = o.dataset.val;
    opts[o.dataset.opt] = v === "true" ? true : v === "false" ? false : v;
    renderOpts();
    return;
  }
  const b = e.target.closest("button[data-act]");
  if (!b) return;
  if (b.dataset.job) jobAction(b);
  else if (b.dataset.act === "testpage") testPage(b);
});

// --- printing a file ---
// The choices live in the #opts fragment as JSON; what was picked stays here
// across swaps. A choice that the queue does not offer falls back to its default.
const opts = { printer: "", sides: "", color: "", hold: false };
const maxUpload = 200 * 1024 * 1024;

function seg(key, choices) {
  const group = document.createElement("div");
  group.className = "seg";
  group.setAttribute("role", "group");
  for (const [v, label] of choices) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = opts[key] === v ? "on" : "";
    b.dataset.opt = key;
    b.dataset.val = String(v);
    b.setAttribute("aria-pressed", String(opts[key] === v));
    b.textContent = label;
    group.append(b);
  }
  return group;
}

function renderOpts() {
  const el = $("opts");
  if (!el) return;
  let printers = [];
  try {
    printers = JSON.parse(el.dataset.printers || "[]");
  } catch (err) {
    printers = [];
  }
  if (!printers.some((p) => p.name === opts.printer)) opts.printer = el.dataset.default || printers[0]?.name || "";
  const p = printers.find((x) => x.name === opts.printer);
  if (!p) {
    el.replaceChildren();
    return;
  }
  const sides = p.sides || [];
  const colors = p.color || [];
  if (!sides.includes(opts.sides)) opts.sides = p.sidesDefault || sides[0] || "";
  if (!colors.includes(opts.color)) opts.color = p.colorDefault || colors[0] || "";
  const two = sides.includes("two-sided-long-edge") ? "two-sided-long-edge" : sides.find((s) => s.startsWith("two-sided"));
  if (opts.sides === "two-sided-short-edge" && two === "two-sided-long-edge") opts.sides = two;
  el.replaceChildren(
    seg("printer", printers.map((x) => [x.name, x.label])),
    ...(two ? [seg("sides", [["one-sided", "1-sided"], [two, "2-sided"]])] : []),
    ...(colors.includes("color") ? [seg("color", [["color", "color"], ["monochrome", "gray"]])] : []),
    seg("hold", [[false, "print now"], [true, "hold first"]]),
  );
}

async function printFile(file) {
  if (file.size > maxUpload) {
    say("Files up to 200 MB can be printed from here.", true);
    return;
  }
  if (!opts.printer) {
    say("There is no printer to send it to.", true);
    return;
  }
  // The server reads the form as it arrives, so the file has to come last.
  const form = new FormData();
  form.append("printer", opts.printer);
  if (opts.sides) form.append("sides", opts.sides);
  if (opts.color) form.append("color", opts.color);
  form.append("hold", String(opts.hold));
  form.append("file", file);
  say(`Sending "${file.name}".`);
  await send("/print", form);
}

$("choose").onclick = () => $("file").click();
$("file").onchange = (e) => {
  const f = e.target.files[0];
  e.target.value = "";
  if (f) printFile(f);
};
document.addEventListener("keydown", (e) => {
  if (e.key === "o" && !e.ctrlKey && !e.metaKey && !e.altKey && $("dialog").hidden &&
      !/INPUT|TEXTAREA/.test(document.activeElement.tagName)) {
    $("file").click();
  }
});
const drop = $("drop");
const hasFiles = (e) => e.dataTransfer?.types.includes("Files");
document.addEventListener("dragover", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  drop.classList.add("over");
});
document.addEventListener("dragleave", (e) => {
  if (!e.relatedTarget) drop.classList.remove("over");
});
document.addEventListener("drop", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  drop.classList.remove("over");
  const f = e.dataTransfer.files[0];
  if (f) printFile(f);
});

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
readServerMsg();
renderOpts();
watch();
