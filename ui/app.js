"use strict";

const el = (id) => document.getElementById(id);

// JS failures inside the webview are invisible from the terminal; forward
// them to Go so -debug shows them as event=ui_error lines.
window.addEventListener("error", (ev) => {
  window.logError?.(String(ev.message || ev.error));
});
window.addEventListener("unhandledrejection", (ev) => {
  window.logError?.(String(ev.reason));
});

// Keyboard zoom: Cmd+/Cmd-/Cmd0 (Ctrl elsewhere) scale the root font size,
// and everything sized in rem follows. Persisted per app.
const ZOOM_KEY = "sr-dash.zoom";
let zoom = Number(localStorage.getItem(ZOOM_KEY)) || 100;

function applyZoom() {
  document.documentElement.style.fontSize = zoom + "%";
}

window.addEventListener("keydown", (ev) => {
  if (!(ev.metaKey || ev.ctrlKey) || ev.altKey) return;
  switch (ev.key) {
    case "+":
    case "=":
      zoom = Math.min(zoom + 10, 200);
      break;
    case "-":
      zoom = Math.max(zoom - 10, 60);
      break;
    case "0":
      zoom = 100;
      break;
    default:
      return;
  }
  ev.preventDefault();
  localStorage.setItem(ZOOM_KEY, String(zoom));
  applyZoom();
});

applyZoom();

let state = { path: "", exists: false, error: "", connections: [] };
// editingIndex: null = form closed, -1 = adding, >= 0 = editing that entry.
// It only applies to the connections screen.
let editingIndex = null;

// One screen at a time, chosen in the sidebar. Opening the app always lands
// on the dashboard — deliberately not remembered across runs: the monitoring
// screen is the app's home, and a predictable start beats a sticky one. The
// ?screen= override exists for the screenshot harness, which needs to open a
// given screen headlessly; the app itself navigates with no query string.
const SCREENS = ["dashboard", "sessions", "cluster", "connections"];
let screen = new URLSearchParams(location.search).get("screen") || "dashboard";
if (!SCREENS.includes(screen)) screen = "dashboard";

const NAV_ICONS = {
  dashboard: "speedometer",
  sessions: "activity",
  cluster: "diagram",
  connections: "gear",
};

const NAV_COLLAPSED_KEY = "sr-dash.navCollapsed";
let navCollapsed = localStorage.getItem(NAV_COLLAPSED_KEY) === "1";

function goTo(name) {
  screen = name;
  render();
  if (name === "sessions") loadSessions();
  if (name === "cluster") loadCluster();
}

const WINDOW_KEY = "sr-dash.window";
let windowSeconds = Number(localStorage.getItem(WINDOW_KEY)) || 300;
const SLICE_KEY = "sr-dash.sliceBy";
let sliceBy = localStorage.getItem(SLICE_KEY) || "sql";
const TOP_KEY = "sr-dash.topBy";
let topBy = localStorage.getItem(TOP_KEY) || "sql";

// Heading per Top dimension; the tab labels are in the page.
const TOP_TITLES = {
  sql: "Top queries",
  users: "Top users",
  databases: "Top databases",
  resource_groups: "Top resource groups",
  warehouses: "Top warehouses",
  exec_states: "Top exec states",
  fe_hosts: "Top FE hosts",
  catalogs: "Top catalogs",
};

// The series are open-ended (a query text, a user name, a resource group), so
// colors are assigned by name hash from a fixed palette. "Other" is pinned
// gray, the way Performance Insights draws the folded tail. Mid-saturation
// values stay readable on both themes.
const PALETTE = [
  "#1e88e5", "#e53935", "#fb8c00", "#8e24aa", "#26a69a", "#c2185b",
  "#00acc1", "#6d4c41", "#f06292", "#7cb342", "#5c6bc0", "#fdd835",
];

function classColor(name) {
  if (name === "Other") return "#9e9e9e";
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

function button(label, onClick, iconName) {
  const b = document.createElement("button");
  b.type = "button";
  if (iconName) b.append(icon(iconName));
  b.append(label);
  b.addEventListener("click", onClick);
  return b;
}

function buttonRow(...buttons) {
  const row = document.createElement("div");
  row.className = "buttons";
  row.append(...buttons);
  return row;
}

function renderCell(conn, i) {
  const cell = document.createElement("div");
  cell.className = "cell";

  const title = document.createElement("div");
  title.className = "conn-title";
  title.textContent = conn.title;
  if (i === 0) {
    const badge = document.createElement("span");
    badge.className = "conn-default";
    badge.textContent = "default";
    title.append(badge);
  }
  if (i === state.active) {
    const badge = document.createElement("span");
    badge.className = "conn-default";
    badge.textContent = "connected";
    title.append(badge);
  }

  const dsn = document.createElement("div");
  dsn.className = "conn-dsn";
  dsn.textContent = conn.dsn || "";

  const edit = button("", () => openForm(i), "pencil");
  edit.classList.add("icon-only");
  edit.title = "Edit";
  edit.setAttribute("aria-label", "Edit");

  const row = buttonRow(edit);
  if (i !== 0) {
    // "Default" is a position in the file, not a flag: promoting one moves its
    // line to the top, which is where sr-dash looks when it opens.
    row.append(button("Make default", () => makeDefaultIndex(i)));
  }
  if (i === state.active) {
    row.append(button("Disconnect", disconnectActive, "plug"));
  } else {
    // Attaching here detaches from the current one: exactly one open server,
    // so a command can never land on the database you were not looking at.
    row.append(button("Connect", () => connectToIndex(i), "plug"));
  }
  cell.append(title, dsn, row);
  return cell;
}

async function makeDefaultIndex(index) {
  showListResult("");
  try {
    state = await window.makeDefault(index);
  } catch (err) {
    showListResult("Could not promote: " + err);
    return;
  }
  render();
}

// Closing the connection is a resting state, not a failure: sr-dash stays on
// this screen attached to nothing until you pick a database again.
async function disconnectActive() {
  showListResult("");
  try {
    state = await window.disconnect();
  } catch (err) {
    showListResult("Could not disconnect: " + err);
    return;
  }
  render();
}

async function connectToIndex(index) {
  showListResult("");
  try {
    state = await window.connectTo(index);
  } catch (err) {
    showListResult("Could not connect: " + err);
    return;
  }
  goTo("dashboard");
}

function render() {
  const sections = ["setup", "list", "dashboard", "sessions", "cluster"];
  const hideAll = () => sections.forEach((id) => { el(id).hidden = true; });

  el("config-error-box").hidden = !state.error;
  if (state.error) {
    el("config-error").textContent = state.error;
    hideAll();
    return;
  }

  el("cfg-path").textContent = state.path;

  // What decides the screen is the RESULT of running the config: no databases
  // configured means sr-dash asks for one, whether the file is missing,
  // empty, or holds only comments.
  const unconfigured = state.connections.length === 0;
  if (unconfigured) {
    screen = "connections";
    if (editingIndex === null) editingIndex = -1;
  }
  const formOpen = screen === "connections" && editingIndex !== null;

  hideAll();
  // Every screen is its own section except connections, which has two faces:
  // the list, and the add/edit form that takes over when one is open.
  let section = screen;
  if (screen === "connections") section = "list";
  if (formOpen) section = "setup";
  el(section).hidden = false;

  el("first-run-note").hidden = !unconfigured;
  el("cancel").hidden = unconfigured;
  // Deleting is a form of editing: the destructive action lives here, not in
  // the list.
  el("delete").hidden = editingIndex === null || editingIndex < 0;
  el("setup-heading").textContent =
    editingIndex >= 0 ? "Edit connection" : "Add a StarRocks connection";

  for (const b of document.querySelectorAll(".nav-item[data-screen]")) {
    if (b.dataset.screen === screen) {
      b.setAttribute("aria-current", "page");
    } else {
      b.removeAttribute("aria-current");
    }
  }
  applyNarrow();

  const attached = state.connections[state.active];
  el("attached").hidden = !attached;
  if (attached) {
    el("attached-name").textContent = attached.title;
    el("attached").title = "Connected to " + attached.title +
      ". Only one connection is open at a time.";
  }

  const box = el("connections");
  box.replaceChildren();
  state.connections.forEach((conn, i) => box.append(renderCell(conn, i)));

  if (!el("dashboard").hidden) refreshDashboard();
}

function showResult(text) {
  el("result").textContent = text;
}

// The list and the edit form are never on screen together, so a failure in the
// list needs its own place to be seen; #result lives inside the form.
function showListResult(text) {
  el("list-result").textContent = text;
}

async function openForm(index) {
  editingIndex = index;
  el("confirm").hidden = true;
  showResult("");
  el("title").value = "";
  el("dsn").value = "";
  if (index >= 0) {
    try {
      el("dsn").value = await window.connectionDSN(index);
    } catch (err) {
      showResult(String(err));
    }
    el("title").value = state.connections[index].title;
  }
  render();
  el("dsn").focus();
}

function closeForm() {
  editingIndex = null;
  el("confirm").hidden = true;
  el("dsn").value = "";
  el("title").value = "";
  showResult("");
  render();
}

async function removeConnection(i) {
  try {
    state = await window.deleteConnection(i);
  } catch (err) {
    el("confirm").hidden = true;
    showResult("Could not remove: " + err);
    return;
  }
  closeForm();
}

el("test").addEventListener("click", async () => {
  const dsn = el("dsn").value.trim();
  if (!dsn) {
    showResult("Enter a connection string first.");
    return;
  }
  showResult("Connecting...");
  const r = await window.testConnection(dsn);
  if (r.ok) {
    showResult("Connected.\n" + r.version);
    return;
  }
  showResult("Connection failed: " + r.error);
});

el("setup-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const dsn = el("dsn").value.trim();
  const title = el("title").value.trim();
  if (!dsn) {
    showResult("Enter a connection string first.");
    return;
  }
  try {
    if (editingIndex >= 0) {
      state = await window.updateConnection(editingIndex, dsn, title);
    } else {
      state = await window.addConnection(dsn, title);
    }
  } catch (err) {
    showResult("Could not save: " + err);
    return;
  }
  closeForm();
});

// --- Dashboard ---

let dash = null;

async function refreshDashboard() {
  if (el("dashboard").hidden || paused) return;
  try {
    dash = await window.dashboardState(windowSeconds, sliceBy, topBy);
  } catch (err) {
    el("dash-status").textContent = String(err);
    return;
  }
  el("dash-title").textContent = dash.title || "";
  // Never leave the user guessing: idle-and-sampling must not look broken.
  let status = dash.status;
  if (dash.connected) {
    status = "sampling, " + dash.samples + " samples in this window";
  }
  el("dash-status").textContent = status;
  for (const b of document.querySelectorAll(".win")) {
    b.setAttribute("aria-pressed", String(Number(b.dataset.window) === windowSeconds));
  }
  for (const b of document.querySelectorAll(".tab")) {
    b.setAttribute("aria-pressed", String(b.dataset.top === topBy));
  }
  el("top-heading").firstChild.textContent = (TOP_TITLES[topBy] || "Top") + " ";
  drawChart();
  drawCounters();
  renderLegend();
  renderTopSQL();
}

const CHART_PAD = { left: 40, right: 8, top: 8, bottom: 18 };

// drawStacked renders one stacked-column chart in the AWS PI look: thin bars
// with gaps, integer grid, a few HH:MM ticks. Returns the data maximum so
// callers can annotate empty charts.
function drawStacked(canvas, classes, buckets, opts = {}) {
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  const ctx = canvas.getContext("2d");
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);

  const textColor = getComputedStyle(document.body).color;
  const pad = CHART_PAD;
  const plotW = w - pad.left - pad.right;
  const plotH = h - pad.top - pad.bottom;

  let dataMax = 0;
  for (const b of buckets) {
    let total = 0;
    for (const k in b.v) total += b.v[k];
    if (total > dataMax) dataMax = total;
  }
  let yMax = Math.max(1, Math.ceil(dataMax));
  // A reference line joins the scale only when it would not flatten the data.
  const ref = opts.refValue || 0;
  if (ref > 0 && ref <= dataMax * 4) yMax = Math.max(yMax, Math.ceil(ref));

  ctx.font = "11px system-ui";
  ctx.strokeStyle = "rgba(127,127,127,0.3)";
  ctx.fillStyle = textColor;
  const step = Math.max(1, Math.ceil(yMax / 5));
  for (let y = 0; y <= yMax; y += step) {
    const py = pad.top + plotH - (y / yMax) * plotH;
    ctx.beginPath();
    ctx.moveTo(pad.left, py);
    ctx.lineTo(w - pad.right, py);
    ctx.stroke();
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    ctx.fillText(String(y), pad.left - 6, py);
  }

  const n = buckets.length;
  const slotW = plotW / n;
  const barW = Math.max(1, slotW * 0.6);
  for (let i = 0; i < n; i++) {
    const b = buckets[i];
    let y = pad.top + plotH;
    const x = pad.left + i * slotW + (slotW - barW) / 2;
    for (const cls of classes) {
      const v = b.v[cls];
      if (!v) continue;
      const barH = (v / yMax) * plotH;
      y -= barH;
      ctx.fillStyle = (opts.colorOf || classColor)(cls);
      ctx.fillRect(x, y, barW, barH);
    }
  }

  if (ref > 0 && ref <= yMax) {
    const py = pad.top + plotH - (ref / yMax) * plotH;
    ctx.save();
    ctx.strokeStyle = textColor;
    ctx.setLineDash([5, 4]);
    ctx.globalAlpha = 0.6;
    ctx.beginPath();
    ctx.moveTo(pad.left, py);
    ctx.lineTo(w - pad.right, py);
    ctx.stroke();
    ctx.restore();
  }

  ctx.fillStyle = textColor;
  ctx.textBaseline = "top";
  ctx.textAlign = "center";
  const fmt = (ms) => new Date(ms).toLocaleTimeString(undefined,
    { hour: "2-digit", minute: "2-digit" });
  for (let i = 0; i < n; i += Math.floor(n / 4)) {
    ctx.fillText(fmt(buckets[i].t), pad.left + (i + 0.5) * slotW,
      pad.top + plotH + 4);
  }
  ctx.textAlign = "right";
  ctx.fillText("now", w - pad.right, pad.top + plotH + 4);

  return dataMax;
}

function drawChart() {
  const canvas = el("chart");
  const maxCPU = drawStacked(canvas, dash.classes, dash.buckets);

  // An empty chart while sampling means the cluster is idle; say so instead
  // of looking broken.
  if (maxCPU === 0 && dash.connected && dash.samples > 0) {
    const ctx = canvas.getContext("2d");
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    ctx.globalAlpha = 0.7;
    ctx.fillStyle = getComputedStyle(document.body).color;
    ctx.font = "12px system-ui";
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.fillText("No CPU time sampled, the cluster looks idle.",
      w / 2, h / 2);
    ctx.globalAlpha = 1;
  }
}

// Fixed colors for the counter charts. The series names are unique across
// the panels, so one map covers them all.
const COUNTER_COLORS = {
  "query": "#1e88e5",
  "idle": "#9e9e9e",
  "other": "#8e24aa",
  "queries/s": "#1e88e5",
  "errors/s": "#e53935",
  "pending": "#fb8c00",
  "running slots": "#26a69a",
  "backend %": "#8e24aa",
  "query %": "#00acc1",
  "disk read/s": "#1e88e5",
  "scanned/s": "#26a69a",
  "compacted/s": "#7cb342",
  "running": "#5c6bc0",
};

function counterColor(name) {
  return COUNTER_COLORS[name] || "#78909c";
}

// The panels are described by the Go side, so the list of them and their
// series lives in one place rather than twice.
function buildCounterPanels() {
  const box = el("counters");
  box.replaceChildren();
  for (const chart of dash.counterCharts) {
    const panel = document.createElement("div");
    panel.className = "counter-panel";

    const heading = document.createElement("h3");
    heading.textContent = chart.title;
    if (chart.note) {
      const note = document.createElement("span");
      note.className = "hint";
      note.id = "counter-note-" + chart.id;
      heading.append(note);
    }

    const canvas = document.createElement("canvas");
    canvas.id = "counter-" + chart.id;

    const legend = document.createElement("div");
    legend.className = "mini-legend";
    legend.id = "counter-legend-" + chart.id;

    panel.append(heading, canvas, legend);
    box.append(panel);
  }
}

function drawCounters() {
  const box = el("counters");
  if (box.childElementCount !== dash.counterCharts.length) {
    buildCounterPanels();
  }
  for (const chart of dash.counterCharts) {
    drawStacked(el("counter-" + chart.id), chart.series, chart.buckets, {
      colorOf: counterColor,
    });
    renderMiniLegend("counter-legend-" + chart.id, chart.series, counterColor);
    const note = el("counter-note-" + chart.id);
    if (note) {
      note.textContent = " · " + chart.note + ": " + Number(chart.noteValue).toFixed(2);
    }
  }
}

function renderMiniLegend(id, classes, colorOf) {
  const box = el(id);
  box.replaceChildren();
  for (const cls of classes) {
    const item = document.createElement("span");
    item.className = "legend-item";
    const swatch = document.createElement("span");
    swatch.className = "legend-swatch";
    swatch.style.background = colorOf(cls);
    item.append(swatch, cls);
    box.append(item);
  }
}

function renderLegend() {
  const box = el("legend");
  box.replaceChildren();
  for (const cls of dash.classes) {
    const item = document.createElement("span");
    item.className = "legend-item";
    // A SQL slice key is a whole statement: the row is clipped, the full
    // text stays reachable on hover.
    item.title = cls;
    const swatch = document.createElement("span");
    swatch.className = "legend-swatch";
    swatch.style.background = classColor(cls);
    item.append(swatch, cls);
    box.append(item);
  }
}

function renderTopSQL() {
  const box = el("topsql");
  box.replaceChildren();
  for (const q of dash.topSQL) {
    const cell = document.createElement("div");
    cell.className = "cell";
    const load = document.createElement("div");
    load.className = "sql-load";
    load.textContent = q.cpuSec.toFixed(2) + " CPU sec (" +
      q.pct.toFixed(1) + "%)";
    // The bar spans the entry's share of the window and splits it by the
    // chart's series, in the chart colors, like Performance Insights.
    const bar = document.createElement("div");
    bar.className = "sql-bar";
    bar.style.width = Math.max(1, q.pct) + "%";
    for (const cls of dash.classes) {
      const v = q.byClass[cls];
      if (!v) continue;
      const seg = document.createElement("span");
      seg.style.background = classColor(cls);
      seg.style.flexGrow = v;
      seg.title = cls;
      bar.append(seg);
    }
    // A statement gets monospace and wrapping; a user or resource group name
    // is a short label and reads better as plain text.
    const text = document.createElement("div");
    text.className = topBy === "sql" ? "sql-text" : "top-label";
    text.textContent = q.query;
    cell.append(load, bar, text);
    box.append(cell);
  }
  if (dash.topSQL.length === 0) {
    const note = document.createElement("p");
    note.className = "hint";
    note.textContent = "No CPU time sampled in this window yet.";
    box.append(note);
  }
}

// Hover tooltip: time plus the per-series values of the bucket under the
// cursor, PI style.
el("chart").addEventListener("mousemove", (ev) => {
  const tip = el("chart-tip");
  if (!dash || dash.buckets.length === 0) {
    tip.hidden = true;
    return;
  }
  const rect = el("chart").getBoundingClientRect();
  const pad = CHART_PAD;
  const plotW = rect.width - pad.left - pad.right;
  const x = ev.clientX - rect.left - pad.left;
  if (x < 0 || x > plotW) {
    tip.hidden = true;
    return;
  }
  const i = Math.min(dash.buckets.length - 1,
    Math.floor((x / plotW) * dash.buckets.length));
  const b = dash.buckets[i];

  // PI tooltip shape: each series as "value, pct%", then the total.
  tip.replaceChildren();
  const when = document.createElement("div");
  when.textContent = new Date(b.t).toLocaleTimeString();
  tip.append(when);
  let total = 0;
  for (const cls of dash.classes) total += b.v[cls] || 0;
  for (const cls of dash.classes) {
    const v = b.v[cls];
    if (!v) continue;
    const pct = total > 0 ? Math.round(100 * v / total) : 0;
    const line = document.createElement("div");
    const swatch = document.createElement("span");
    swatch.className = "legend-swatch";
    swatch.style.background = classColor(cls);
    line.append(swatch, cls + ": " + v.toFixed(2) + ", " + pct + "%");
    tip.append(line);
  }
  const sum = document.createElement("div");
  sum.textContent = "Total CPU: " + total.toFixed(2) + " sec/sec";
  tip.append(sum);

  tip.hidden = false;
  const tipX = Math.min(ev.clientX - rect.left + 12,
    rect.width - tip.offsetWidth - 4);
  tip.style.left = Math.max(0, tipX) + "px";
  tip.style.top = (ev.clientY - rect.top + 12) + "px";
});
el("chart").addEventListener("mouseleave", () => {
  el("chart-tip").hidden = true;
});

// Auto-refresh pause: the sampler keeps collecting in Go; only the screen
// freezes for inspection.
let paused = false;

function renderPause() {
  const b = el("pause");
  b.replaceChildren(icon(paused ? "play-fill" : "pause-fill"));
  const label = paused ? "Resume auto-refresh" : "Pause auto-refresh";
  b.title = label;
  b.setAttribute("aria-label", label);
}

el("pause").addEventListener("click", () => {
  paused = !paused;
  renderPause();
  if (!paused) refreshDashboard();
});
renderPause();

for (const b of document.querySelectorAll(".win")) {
  b.addEventListener("click", () => {
    windowSeconds = Number(b.dataset.window);
    localStorage.setItem(WINDOW_KEY, String(windowSeconds));
    paused = false;
    renderPause();
    refreshDashboard();
  });
}

for (const b of document.querySelectorAll(".tab")) {
  b.addEventListener("click", () => {
    topBy = b.dataset.top;
    localStorage.setItem(TOP_KEY, topBy);
    paused = false;
    renderPause();
    refreshDashboard();
  });
}

el("slice-by").value = sliceBy;
el("slice-by").addEventListener("change", () => {
  sliceBy = el("slice-by").value;
  localStorage.setItem(SLICE_KEY, sliceBy);
  paused = false;
  renderPause();
  refreshDashboard();
});

// --- Sessions screen ---

let sessions = null;
// sessConfirming is the connection id whose kill is waiting for a
// confirmation.
let sessConfirming = null;

async function loadSessions() {
  el("sess-status").textContent = "Loading sessions...";
  try {
    sessions = await window.sessionList();
  } catch (err) {
    el("sess-status").textContent = String(err);
    return;
  }
  renderSessions();
}

function renderSessions() {
  if (!sessions) return;
  el("sess-status").textContent = sessions.error || "";

  const box = el("sess-list");
  box.replaceChildren();

  let rows = (sessions.rows || []).slice();
  if (el("sess-active-only").checked) {
    rows = rows.filter((s) => s.command === "Query");
  }
  // The busiest first: a session that has been running the longest is the
  // one worth looking at.
  rows.sort((a, b) => (b.seconds || 0) - (a.seconds || 0));

  for (const s of rows) {
    box.append(renderSession(s));
  }
  if (rows.length === 0) {
    const note = document.createElement("p");
    note.className = "hint";
    note.textContent = "No sessions on this frontend.";
    box.append(note);
  }
}

function renderSession(s) {
  const cell = document.createElement("div");
  cell.className = "cell";

  const head = document.createElement("div");
  head.className = "conn-title";
  head.textContent = s.user + " · " + s.id;

  const meta = document.createElement("div");
  meta.className = "hint";
  meta.textContent = [s.host, s.db ? "db " + s.db : "", s.command,
    s.seconds + "s", s.state].filter(Boolean).join(" · ");

  const info = document.createElement("div");
  info.className = "sql-text";
  info.textContent = s.info || "(no statement)";

  cell.append(head, meta, info);

  if (sessConfirming === s.id) {
    // The exact statement, before it runs.
    const stmt = document.createElement("div");
    stmt.className = "sql-text";
    stmt.textContent = "KILL " + s.id;
    const act = button("Confirm kill", () => runKill(s));
    act.classList.add("destructive");
    cell.append(stmt, buttonRow(
      button("Back", () => {
        sessConfirming = null;
        renderSessions();
      }),
      act,
    ));
    return cell;
  }

  const kill = button("Kill connection...", () => {
    sessConfirming = s.id;
    renderSessions();
  });
  kill.classList.add("destructive");
  cell.append(buttonRow(kill));
  return cell;
}

async function runKill(s) {
  sessConfirming = null;
  try {
    await window.killSession(Number(s.id));
  } catch (err) {
    el("sess-status").textContent = String(err);
    return;
  }
  loadSessions();
}

el("sess-refresh").addEventListener("click", loadSessions);
el("sess-active-only").addEventListener("change", renderSessions);

// --- Cluster screen ---

let cluster = null;

async function loadCluster() {
  el("cluster-status").textContent = "Reading the cluster...";
  try {
    cluster = await window.clusterState();
  } catch (err) {
    el("cluster-status").textContent = String(err);
    return;
  }
  renderCluster();
}

function renderCluster() {
  if (!cluster) return;
  el("cluster-status").textContent = cluster.error || "";

  const totals = el("cluster-totals");
  totals.replaceChildren();
  if (!cluster.error) {
    const s = cluster.stat;
    const facts = [
      s.databases + " databases",
      s.tables + " tables",
      s.tablets + " tablets",
      s.replicas + " replicas",
    ];
    const unhealthy = s.unhealthy + s.inconsistent + s.cloning + s.errorState;
    if (unhealthy > 0) {
      facts.push(s.unhealthy + " unhealthy, " + s.inconsistent + " inconsistent, " +
        s.cloning + " cloning, " + s.errorState + " in error");
    }
    const p = document.createElement("p");
    p.className = unhealthy > 0 ? "warn" : "hint";
    p.textContent = facts.join(" · ");
    totals.append(p);
  }

  const fes = el("cluster-frontends");
  fes.replaceChildren();
  const feHead = ["Id", "Name", "IP", "Role", "Alive", "Journal", "Heartbeat", "Version"];
  fes.append(dataTable(feHead, (cluster.frontends || []).map((f) => [
    f.id, f.name, f.ip, f.role, f.alive ? "yes" : "no", f.journalId,
    f.heartbeat, f.version,
  ])));

  const bes = el("cluster-backends");
  bes.replaceChildren();
  const beHead = ["Id", "IP", "Alive", "Cores", "CPU %", "Memory", "Disk", "Tablets",
    "Queries", "Heartbeat", "Status"];
  bes.append(dataTable(beHead, (cluster.backends || []).map((b) => [
    b.id, b.ip, b.alive ? "yes" : "no", b.cpuCores, b.cpuUsedPct,
    bytes(b.memLimit) + " · " + b.memUsedPct.toFixed(1) + "%",
    bytes(b.diskUsed) + " / " + bytes(b.diskTotal) + " · " + b.diskUsedPct.toFixed(1) + "%",
    b.tabletNum, b.runningQueries, b.heartbeat,
    b.errMsg ? b.errMsg : (b.statusCode || ""),
  ])));
}

// dataTable builds a table from a header and the rows.
function dataTable(head, rows) {
  const table = document.createElement("table");
  table.className = "data-table";
  const thead = document.createElement("thead");
  const htr = document.createElement("tr");
  for (const h of head) {
    const th = document.createElement("th");
    th.textContent = h;
    htr.append(th);
  }
  thead.append(htr);
  table.append(thead);

  const tbody = document.createElement("tbody");
  for (const row of rows) {
    const tr = document.createElement("tr");
    for (const cell of row) {
      const td = document.createElement("td");
      td.textContent = cell === null || cell === undefined ? "" : String(cell);
      tr.append(td);
    }
    tbody.append(tr);
  }
  table.append(tbody);
  if (rows.length === 0) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = head.length;
    td.className = "hint";
    td.textContent = "Nothing to show.";
    tr.append(td);
    tbody.append(tr);
  }
  return table;
}

// bytes formats a byte count the way the server does.
function bytes(n) {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return (i === 0 ? v.toFixed(0) : v.toFixed(2)) + " " + units[i];
}

el("cluster-refresh").addEventListener("click", loadCluster);

// --- Sidebar ---

for (const b of document.querySelectorAll(".nav-item[data-screen]")) {
  const span = document.createElement("span");
  span.textContent = b.textContent;
  b.replaceChildren(icon(NAV_ICONS[b.dataset.screen] || "list"), span);
  b.addEventListener("click", () => goTo(b.dataset.screen));
}

const collapseBtn = el("nav-collapse");
collapseBtn.replaceChildren(icon("list"));
collapseBtn.addEventListener("click", () => {
  navCollapsed = !navCollapsed;
  localStorage.setItem(NAV_COLLAPSED_KEY, navCollapsed ? "1" : "0");
  render();
});

// A narrow window collapses the rail on its own and restores the user's
// choice when there is room again: their preference is remembered, not
// overwritten by a resize.
const NARROW = window.matchMedia("(max-width: 60rem)");

function applyNarrow() {
  const forced = NARROW.matches;
  document.body.classList.toggle("nav-forced-collapsed", forced);
  collapseBtn.disabled = forced;
  document.body.classList.toggle("nav-collapsed", forced || navCollapsed);
  collapseBtn.setAttribute("aria-expanded",
    String(!(forced || navCollapsed)));
}

NARROW.addEventListener("change", applyNarrow);

el("sess-refresh").prepend(icon("arrow-clockwise"));
el("test").prepend(icon("plug"));
el("add").prepend(icon("plus-lg"));
el("delete").prepend(icon("trash"));

setInterval(refreshDashboard, 2000);

(async () => {
  state = await window.configState();
  render();
})();
