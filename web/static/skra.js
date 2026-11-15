// SPDX-License-Identifier: AGPL-3.0-or-later
"use strict";

// The server puts its token into the page; every POST sends it back. The
// token is kept in the state directory, so it outlives an idle exit.
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

// A note goes after a few seconds, unless hold is set: that one stays until
// the next say(), for a wait whose end is the answer.
function say(text, bad = false, hold = false) {
  note = { text, bad };
  showMsg();
  clearTimeout(noteTimer);
  if (hold) return;
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
const slots = ["printers", "opts", "queue", "queues", "recent", "history", "need", "foot"];

function swap(id, html) {
  const el = $(id);
  if (!el) return;
  el.outerHTML = html;
  if (id === "foot") readServerMsg();
  if (id === "opts") renderOpts();
  if (id === "queues") markSelected();
}

function connect() {
  if (source) return;
  // The history and scan tabs ask for their own fragments only.
  const page = document.body.dataset.page;
  source = new EventSource(page === "history" || page === "scan" ? `/events?page=${page}` : "/events");
  for (const id of slots) {
    source.addEventListener(id, (e) => swap(id, e.data));
  }
  if (page === "scan") source.addEventListener("scan", (e) => scanEvent(e.data));
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

// parseList reads the JSON list in a data attribute; a missing or broken one
// is an empty list.
function parseList(raw) {
  try {
    return JSON.parse(raw || "[]");
  } catch (err) {
    return [];
  }
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
// sendRaw answers the Response, or null when skrá could not be reached; send
// answers whether the server did it.
async function sendRaw(url, body, button) {
  if (button) button.disabled = true;
  try {
    const res = await post(url, body);
    say((await res.text()).trim() || (res.ok ? "Done." : `skrá answered ${res.status}.`), !res.ok);
    return res;
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
    return null;
  } finally {
    if (button?.isConnected) button.disabled = false;
  }
}

async function send(url, body, button) {
  return (await sendRaw(url, body, button))?.ok ?? false;
}

async function moveJob(b) {
  const { job } = b.dataset;
  const targets = parseList(b.dataset.targets);
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
  const pick = e.target.closest("#cfg [data-pick], #add [data-pick]");
  if (pick) {
    for (const x of pick.parentElement.children) {
      x.classList.toggle("on", x === pick);
      x.setAttribute("aria-pressed", String(x === pick));
    }
    if (pick.parentElement.dataset.group === "driver") showDrivers(pick.dataset.pick === "choose");
    return;
  }
  const o = e.target.closest("[data-opt]");
  if (o) {
    const v = o.dataset.val;
    opts[o.dataset.opt] = v === "true" ? true : v === "false" ? false : v;
    renderOpts();
    return;
  }
  const b = e.target.closest("button[data-act]");
  if (!b) return;
  if (b.dataset.act.startsWith("log-")) logAction(b);
  else if (b.dataset.job) jobAction(b);
  else if (b.dataset.act === "testpage") testPage(b);
  else if (b.dataset.q) queueAction(b);
  else addAction(b);
});

// --- the printers tab ---
// The queue table is swapped by the event stream; the settings panel under it
// is not, so what is typed into it stays until it is saved or closed. It is
// loaded from the server as HTML and loaded again after a save. No instant
// action reloads it: those live in the table and on the overview cards.
let panelFor = "";

function markSelected() {
  for (const tr of document.querySelectorAll("#queues tr[data-q]")) tr.classList.toggle("sel", tr.dataset.q === panelFor);
}

async function loadPanel(name) {
  const panel = $("panel");
  if (!panel) return;
  try {
    const res = await fetch(`/printers/${encodeURIComponent(name)}/settings`, { cache: "no-store" });
    const text = await res.text();
    if (!res.ok) {
      say(text.trim() || `skrá answered ${res.status}.`, true);
      return;
    }
    panel.innerHTML = text;
    panelFor = name;
    markSelected();
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
  }
}

function closePanel() {
  panelFor = "";
  $("panel")?.replaceChildren();
  markSelected();
}

// settingsForm reads the fields of the panel that differ from what the panel
// was loaded with (data-was): the fields by name and the button rows by group.
// The panel is never swapped live, so it can be stale; a field the user did not
// touch is not sent, so a save cannot undo what changed elsewhere meanwhile.
function settingsForm(root) {
  const form = new URLSearchParams();
  for (const el of root.querySelectorAll("input[name], select[name]")) {
    if (el.value !== el.dataset.was) form.set(el.name, el.value);
  }
  for (const g of root.querySelectorAll("[data-group]")) {
    const on = g.querySelector("button.on");
    if (on && on.dataset.pick !== g.dataset.was) form.set(g.dataset.group, on.dataset.pick);
  }
  return form;
}

async function queueAction(b) {
  const { act, q } = b.dataset;
  const base = `/printers/${encodeURIComponent(q)}`;
  const restore = `[data-act="${act}"][data-q="${CSS.escape(q)}"]`;
  switch (act) {
    case "configure":
      if (panelFor === q) closePanel();
      else await loadPanel(q);
      return;
    case "close":
      closePanel();
      return;
    case "save": {
      const root = $("cfg");
      if (root?.dataset.q !== q) return;
      const form = settingsForm(root);
      if (form.toString() === "") {
        say("Nothing to save.");
        return;
      }
      if (await send(`${base}/settings`, form, b)) await loadPanel(q);
      return;
    }
    case "remove":
    case "cancel-all": {
      const glyph = act === "remove" ? "\u{F0A7A}" : "\u{F0156}";
      if (!(await confirmBox(b.dataset.confirmTitle, b.dataset.confirm, b.dataset.confirmLabel, b.dataset.keep, restore, undefined, glyph))) return;
      // The button says "remove"; the route is the CUPS operation, delete.
      if (!(await send(`${base}/${act === "remove" ? "delete" : act}`, undefined, b))) return;
      if (act === "remove" && panelFor === q) closePanel();
      return;
    }
    case "move-all": {
      const targets = parseList(b.dataset.targets);
      if (targets.length === 0) return;
      const to = await confirmBox(b.dataset.moveTitle, b.dataset.moveText, b.dataset.moveLabel, b.dataset.keep, restore,
        targets.map((t) => [t.name, t.label]), "\u{F0054}");
      if (to) await send(`${base}/move-all`, new URLSearchParams({ to }), b);
      return;
    }
    case "command":
      // Only Clean carries confirm texts, because it uses ink.
      if (b.dataset.confirm && !(await confirmBox(b.dataset.confirmTitle, b.dataset.confirm, b.dataset.confirmLabel, b.dataset.keep, restore, undefined, "\u{F05B7}"))) return;
      await send(`${base}/command`, new URLSearchParams({ command: b.dataset.command }), b);
      return;
    default: // default, pause, resume
      await send(`${base}/${act}`, undefined, b);
  }
}


// --- adding a printer ---
// A search asks CUPS, which looks for as long as the button says and answers
// when that is over, so the browser counts the seconds down meanwhile; nothing
// runs by itself. The result is a table of devices. A device (or an address
// typed by hand) opens the add panel, which the server renders; the panel is
// not part of the event stream, so what is typed into it stays.
async function search(b) {
  const box = $("devices");
  let left = Number(b.dataset.seconds) || 10;
  const wait = document.createElement("div");
  const count = document.createElement("span");
  wait.className = "wait";
  count.className = "cd";
  wait.append("looking for printers \u00b7 ", count, " s");
  const show = () => (count.textContent = String(left));
  show();
  box.replaceChildren(wait);
  b.disabled = true;
  const tick = setInterval(() => {
    left = Math.max(0, left - 1);
    show();
  }, 1000);
  try {
    const res = await post("/printers/search");
    const text = await res.text();
    if (res.ok) {
      box.innerHTML = text;
    } else {
      box.replaceChildren();
      say(text.trim() || `skrá answered ${res.status}.`, true);
    }
  } catch (err) {
    box.replaceChildren();
    say("Could not reach skrá. Is it still running?", true);
  } finally {
    clearInterval(tick);
    if (b.isConnected) b.disabled = false;
  }
}

async function openAdd(device, info, model, id) {
  const slot = $("add-panel");
  if (!slot) return;
  const query = new URLSearchParams({ uri: device, info, model, id });
  try {
    const res = await fetch(`/printers/new?${query}`, { cache: "no-store" });
    const text = await res.text();
    if (!res.ok) {
      say(text.trim() || `skrá answered ${res.status}.`, true);
      return;
    }
    slot.innerHTML = text;
    if (!$("ppd-row").hidden) loadDrivers();
    $("a-name").focus();
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
  }
}

// The driver list is loaded when it is first shown and again when the maker
// changes. An answer that comes after a newer question is dropped.
let driversAsked = 0;

async function loadDrivers() {
  const list = $("ppd-list");
  if (!list) return;
  const mine = ++driversAsked;
  const query = new URLSearchParams({ id: document.querySelector('#add input[name="id"]').value, make: $("a-make").value.trim() });
  const wait = document.createElement("span");
  wait.className = "wait";
  wait.textContent = "loading the drivers CUPS has";
  list.replaceChildren(wait);
  let html;
  try {
    const res = await fetch(`/printers/drivers?${query}`, { cache: "no-store" });
    const text = await res.text();
    if (mine !== driversAsked) return;
    if (!res.ok) {
      wait.textContent = `could not list drivers: ${text.trim() || res.status}`;
      return;
    }
    html = text;
  } catch (err) {
    if (mine === driversAsked) wait.textContent = "could not list drivers: skrá does not answer";
    return;
  }
  list.innerHTML = html;
}

function showDrivers(on) {
  const row = $("ppd-row");
  if (!row) return;
  const first = row.hidden && on && !$("ppd-list").firstChild;
  row.hidden = !on;
  if (first) loadDrivers();
}

document.addEventListener("change", (e) => {
  if (e.target.id === "a-make") loadDrivers();
});

// addForm reads every field of the add panel: the fields by name and the
// button rows by group.
function addForm(root) {
  const form = new URLSearchParams();
  for (const el of root.querySelectorAll("input[name], select[name]")) form.set(el.name, el.value);
  for (const g of root.querySelectorAll("[data-group]")) {
    const on = g.querySelector("button.on");
    if (on) form.set(g.dataset.group, on.dataset.pick);
  }
  return form;
}

async function createPrinter(b) {
  const root = $("add");
  if (!root) return;
  const form = addForm(root);
  const name = form.get("name").trim();
  say(form.get("driver") === "everywhere"
    ? `Adding ${name}: CUPS asks the printer what it can do, this takes a few seconds.`
    : `Adding ${name}.`, false, true);
  const res = await sendRaw("/printers/add", form, b);
  // The queue exists after a success, and after a failure that says so.
  if (res && (res.ok || res.headers.get("X-Skra-Added") === "true")) $("add-panel")?.replaceChildren();
}

function addAction(b) {
  switch (b.dataset.act) {
    case "search":
      return search(b);
    case "add-device":
      return openAdd(b.dataset.device, b.dataset.info, b.dataset.model, b.dataset.id);
    case "add-manual": {
      const uri = $("manual-uri").value.trim();
      if (!uri) {
        say("Type the printer's address first. It looks like ipp://printer.local/ipp/print.", true);
        return;
      }
      return openAdd(uri, "", "", "");
    }
    case "cancel-add":
      $("add-panel").replaceChildren();
      return;
    case "create":
      return createPrinter(b);
  }
}
// --- the CUPS log ---
// The log section is loaded once when the tab opens and again on "refresh log"
// and after the level was set; the event stream does not carry it, because a
// journal that moves under the reader is hard to read. An answer that comes
// after a newer question is dropped.
let logAsked = 0;
let logBusy = false;

async function loadLog() {
  const slot = $("log");
  if (!slot) return;
  const mine = ++logAsked;
  try {
    const res = await fetch("/printers/log", { cache: "no-store" });
    const text = await res.text();
    if (mine !== logAsked) return;
    if (!res.ok) {
      say(text.trim() || `skrá answered ${res.status}.`, true);
      return;
    }
    slot.innerHTML = text;
    const box = $("logbox");
    if (box) box.scrollTop = box.scrollHeight;
  } catch (err) {
    if (mine === logAsked) say("Could not reach skrá. Is it still running?", true);
  }
}

async function logAction(b) {
  if (b.dataset.act === "log-refresh") {
    b.disabled = true;
    await loadLog();
    if (b.isConnected) b.disabled = false;
    return;
  }
  const { level } = b.dataset;
  if (logBusy || b.classList.contains("on")) return;
  logBusy = true;
  say(`Setting the log level to ${level}. CUPS restarts its scheduler, this takes a few seconds.`, false, true);
  try {
    await send("/printers/log/level", new URLSearchParams({ level }), b);
    // Whatever the answer was, the section shows what cupsd.conf says now.
    await loadLog();
  } finally {
    logBusy = false;
  }
}

if ($("log")) loadLog();

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
  const printers = parseList(el.dataset.printers);
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

// The printers tab has no print form: its elements are not there.
if ($("choose")) $("choose").onclick = () => $("file").click();
if ($("file")) {
  $("file").onchange = (e) => {
    const f = e.target.files[0];
    e.target.value = "";
    if (f) printFile(f);
  };
}
document.addEventListener("keydown", (e) => {
  if (e.key === "o" && $("file") && !e.ctrlKey && !e.metaKey && !e.altKey && $("dialog").hidden &&
      !/INPUT|TEXTAREA/.test(document.activeElement.tagName)) {
    $("file").click();
  }
});
const drop = $("drop");
const hasFiles = (e) => drop && e.dataTransfer?.types.includes("Files");
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

// --- the scan tab ---
// No scanner is asked before the tab is open: the scanner list and the form
// are fragments loaded when it opens, and the form again when another scanner
// is picked. A scan runs on the server. The event stream's scan fragment holds
// every scan the service knows; the page shows the one it started, or, when
// it started none, one that runs, and forgets a scan whose session is gone
// because it took no page. While the scan has pages the scanner and the
// resolution stay those of its pages. An answer that comes after a newer
// question is dropped.
let scanSession = "";
let scanRunning = false;
let lastScans = "";
let scanFormAsked = 0;

const scanButton = (which) => document.querySelector(`button[data-scan="${which}"]`);

async function loadScanners() {
  const slot = $("scan-scanners");
  if (!slot) return;
  try {
    const res = await fetch(slot.dataset.src, { cache: "no-store" });
    const text = await res.text();
    if (!res.ok) {
      say(text.trim() || `skrá answered ${res.status}.`, true);
      return;
    }
    slot.innerHTML = text;
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
    return;
  }
  tieScanForm();
  await loadScanForm();
}

async function loadScanForm() {
  const slot = $("scan-form");
  const scanner = $("scan-scanner");
  const mine = ++scanFormAsked;
  slot.replaceChildren();
  showScanButtons();
  if (!scanner) return;
  const wait = document.createElement("div");
  wait.className = "wait";
  wait.textContent = "asking the scanner what it can do";
  slot.replaceChildren(wait);
  try {
    const res = await fetch(`/scan/form?scanner=${encodeURIComponent(scanner.value)}`, { cache: "no-store" });
    const text = await res.text();
    if (mine !== scanFormAsked) return;
    if (!res.ok) {
      slot.replaceChildren();
      say(text.trim() || `skrá answered ${res.status}.`, true);
      return;
    }
    slot.innerHTML = text;
  } catch (err) {
    if (mine !== scanFormAsked) return;
    slot.replaceChildren();
    say("Could not reach skrá. Is it still running?", true);
  }
  showScanButtons();
}

// The scan button waits for a form and for the running scan; it says "scan
// more" once the scan has pages.
function showScanButtons() {
  const start = scanButton("start");
  if (!start) return;
  start.disabled = scanRunning || !document.querySelector("#scan-form [data-group]");
  scanButton("stop").hidden = !scanRunning;
  start.querySelector("span").textContent = $("scan-job").querySelector(".pg") ? "scan more" : "scan";
  tieScanForm();
}

// tieScanForm holds the scanner and the resolution to those of the shown
// scan's pages, which the server keeps for the whole scan, and frees them
// when the scan has none.
function tieScanForm() {
  const job = $("scan-job").querySelector(".scanjob");
  const tied = job && job.dataset.session === scanSession && job.querySelector(".pg") ? job : null;
  const scanner = $("scan-scanner");
  if (scanner) {
    if (tied && [...scanner.options].some((o) => o.value === tied.dataset.scanner)) scanner.value = tied.dataset.scanner;
    scanner.disabled = Boolean(tied);
    scanner.title = tied ? "This scan stays on this scanner. Save it to change." : "";
  }
  for (const b of document.querySelectorAll('#scan-form [data-group="resolution"] button')) {
    const keep = !tied || b.dataset.pick === tied.dataset.dpi;
    b.disabled = !keep;
    b.title = keep ? "" : `This scan is at ${tied.dataset.dpi} dpi. Save it to change.`;
    if (tied) {
      b.classList.toggle("on", b.dataset.pick === tied.dataset.dpi);
      b.setAttribute("aria-pressed", String(b.dataset.pick === tied.dataset.dpi));
    }
  }
}

// scanNote shows the answer to a start beside the form, in the color the
// server names: a scanner that is away is never red.
function scanNote(text, cls) {
  const el = $("scan-note");
  el.textContent = text;
  el.className = cls ? `scanmsg ${cls}` : "scanmsg";
  el.hidden = !text;
}

// scanFields reads the form: the button rows by group, with "feeder · both
// sides" sent as the feeder and duplex.
function scanFields() {
  const form = new URLSearchParams({ session: scanSession, scanner: $("scan-scanner")?.value ?? "" });
  for (const g of document.querySelectorAll("#scan-form [data-group]")) {
    const v = g.querySelector("button.on")?.dataset.pick ?? "";
    if (g.dataset.group === "source") {
      form.set("source", v === "Duplex" ? "Feeder" : v);
      form.set("duplex", String(v === "Duplex"));
    } else {
      form.set(g.dataset.group, v);
    }
  }
  return form;
}

async function startScan(b) {
  b.disabled = true;
  try {
    const res = await post("/scan/start", scanFields());
    const text = (await res.text()).trim();
    if (res.status !== 202) {
      scanNote(text || `skrá answered ${res.status}.`, res.headers.get("X-Skra-Class") ?? "bad");
      return;
    }
    scanNote("");
    scanSession = text;
    scanRunning = true;
    showScans(); // the first news may have come before the answer
  } catch (err) {
    say("Could not reach skrá. Is it still running?", true);
  } finally {
    showScanButtons();
  }
}

function scanEvent(html) {
  lastScans = html;
  showScans();
}

function showScans() {
  const t = document.createElement("template");
  t.innerHTML = lastScans;
  const jobs = [...t.content.querySelectorAll(".scanjob")];
  if (!scanSession) scanSession = jobs.findLast((j) => j.dataset.running === "true")?.dataset.session ?? "";
  const mine = jobs.find((j) => j.dataset.session === scanSession);
  if (!mine) return;
  $("scan-job").replaceChildren(mine);
  scanRunning = mine.dataset.running === "true";
  if (mine.dataset.gone === "true") scanSession = "";
  showScanButtons();
}

document.addEventListener("click", (e) => {
  const pick = e.target.closest("#scan-form [data-pick]");
  if (pick) {
    for (const x of pick.parentElement.children) {
      x.classList.toggle("on", x === pick);
      x.setAttribute("aria-pressed", String(x === pick));
    }
    return;
  }
  const b = e.target.closest("button[data-scan]");
  if (!b) return;
  if (b.dataset.scan === "start") startScan(b);
  else if (b.dataset.scan === "stop" && scanSession) send(`/scan/${encodeURIComponent(scanSession)}/stop`, undefined, b);
  else if (b.dataset.scan === "find") loadScanners();
});
document.addEventListener("change", (e) => {
  if (e.target.id === "scan-scanner") loadScanForm();
});
if ($("scan-scanners")) loadScanners();

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
