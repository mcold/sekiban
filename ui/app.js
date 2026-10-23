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
const ZOOM_KEY = "sekiban.zoom";
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
const SCREENS = ["dashboard", "sessions", "cluster", "browser", "compactions",
  "profile", "loads", "resources", "metrics", "variables", "connections"];
let screen = new URLSearchParams(location.search).get("screen") || "dashboard";
if (!SCREENS.includes(screen)) screen = "dashboard";

const NAV_ICONS = {
  dashboard: "speedometer",
  sessions: "activity",
  cluster: "diagram",
  browser: "table",
  compactions: "stack",
  profile: "clipboard-data",
  loads: "cloud-arrow-down",
  resources: "sliders2-vertical",
  metrics: "wrench",
  variables: "list",
  connections: "gear",
};

const NAV_COLLAPSED_KEY = "sekiban.navCollapsed";
let navCollapsed = localStorage.getItem(NAV_COLLAPSED_KEY) === "1";

function goTo(name) {
  screen = name;
  render();
  if (name === "sessions") loadSessions();
  if (name === "cluster") loadCluster();
  if (name === "browser") loadBrowser();
  if (name === "compactions") loadCompactions();
  if (name === "profile") loadProfile();
  if (name === "loads") loadLoads();
  if (name === "resources") loadResources();
  if (name === "metrics") loadMetrics();
  if (name === "variables") {
    varUserTyped = false;
    loadVariables();
  }
}

const WINDOW_KEY = "sekiban.window";
let windowSeconds = Number(localStorage.getItem(WINDOW_KEY)) || 300;
const SLICE_KEY = "sekiban.sliceBy";
let sliceBy = localStorage.getItem(SLICE_KEY) || "sql";
const TOP_KEY = "sekiban.topBy";
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
    // line to the top, which is where sekiban looks when it opens.
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

// Closing the connection is a resting state, not a failure: sekiban stays on
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
  // Every screen is a section of its own, except connections, which has two
  // faces: the list, and the add/edit form. Deriving the list from SCREENS
  // keeps a new screen from being left out of it, and so from never hiding.
  const sections = ["setup", "list",
    ...SCREENS.filter((name) => name !== "connections")];
  const hideAll = () => sections.forEach((id) => { el(id).hidden = true; });

  el("config-error-box").hidden = !state.error;
  if (state.error) {
    el("config-error").textContent = state.error;
    hideAll();
    return;
  }

  el("cfg-path").textContent = state.path;

  // What decides the screen is the RESULT of running the config: no databases
  // configured means sekiban asks for one, whether the file is missing,
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
function dataTable(head, rows, onPick, keyOf, widths) {
  const table = document.createElement("table");
  table.className = "data-table";
  if (widths) {
    // A fixed layout keeps the numbers on the screen: the columns get the
    // share they are given, and the long one wraps inside it.
    table.style.tableLayout = "fixed";
  }
  const thead = document.createElement("thead");
  const htr = document.createElement("tr");
  head.forEach((h, i) => {
    const th = document.createElement("th");
    th.textContent = h;
    if (widths && widths[i]) th.style.width = widths[i];
    htr.append(th);
  });
  thead.append(htr);
  table.append(thead);

  const tbody = document.createElement("tbody");
  rows.forEach((row, index) => {
    const tr = document.createElement("tr");
    if (onPick) {
      // A row that can be chosen says so, and says which one is chosen.
      tr.classList.add("row-pick");
      const key = keyOf ? keyOf(row, index) : "";
      if (chosen && key && key === chosen.key) {
        tr.classList.add("row-selected");
      }
      tr.addEventListener("click", () => onPick(row, index));
    }
    for (const cell of row) {
      const td = document.createElement("td");
      // A cell is a value, or a value that says how it wants to be drawn:
      // a statement is long and wraps, a count is a number and does not.
      if (cell !== null && typeof cell === "object") {
        td.textContent = cell.text === null || cell.text === undefined ? "" : String(cell.text);
        if (cell.cls) td.className = cell.cls;
        if (cell.title) td.title = cell.title;
      } else {
        td.textContent = cell === null || cell === undefined ? "" : String(cell);
      }
      tr.append(td);
    }
    tbody.append(tr);
  });
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

// --- Browser screen ---

let browser = null;

async function loadBrowser() {
  el("br-status").textContent = "Reading the catalog...";
  try {
    browser = await window.browserTree();
  } catch (err) {
    el("br-status").textContent = String(err);
    return;
  }
  renderBrowser();
}

function renderBrowser() {
  if (!browser) return;
  el("br-status").textContent = browser.error || "";

  const box = el("br-tree");
  box.replaceChildren();
  if (browser.error) return;

  const db = document.createElement("div");
  db.className = "br-db";
  db.textContent = browser.database;
  box.append(db);

  for (const group of browser.groups || []) {
    const head = document.createElement("div");
    head.className = "br-group";
    head.textContent = group.label + " (" + group.objects.length + ")";
    box.append(head);

    if (group.objects.length === 0) {
      const none = document.createElement("div");
      none.className = "hint br-empty";
      none.textContent = "none";
      box.append(none);
      continue;
    }
    for (const obj of group.objects) {
      box.append(browserNode(group, obj));
    }
  }
}

function browserNode(group, obj) {
  const row = document.createElement("div");
  row.className = "br-node";

  const name = document.createElement("button");
  name.type = "button";
  name.className = "br-name";
  name.textContent = obj.name;
  name.title = "Open " + obj.name + " in its own window";
  const open = async () => {
    try {
      await window.openObject(obj.name);
    } catch (err) {
      el("br-status").textContent = String(err);
    }
  };
  name.addEventListener("click", open);
  name.addEventListener("dblclick", open);

  const badge = document.createElement("span");
  badge.className = "br-badge";
  badge.textContent = obj.model || group.label.replace(/s$/, "");

  const size = document.createElement("span");
  size.className = "br-size";
  if (obj.rows > 0) {
    size.textContent = obj.rows.toLocaleString() + " rows";
  }
  if (obj.size > 0) {
    size.textContent += (size.textContent ? " · " : "") + bytes(obj.size);
  }

  row.append(name, badge, size);
  return row;
}

el("br-refresh").addEventListener("click", loadBrowser);
el("comp-refresh").addEventListener("click", loadCompactions);
el("prof-refresh").addEventListener("click", () => loadProfile(false));
el("prof-audit").addEventListener("click", () => loadProfile(true));
for (const [id, key] of [["prof-summary-statement", "summaryStatement"],
  ["prof-list-queryid", "listQueryId"],
  ["prof-list-statement", "listStatement"]]) {
  el(id).addEventListener("input", () => {
    profFilters[key] = el(id).value;
    renderProfile();
  });
}
el("load-refresh").addEventListener("click", loadLoads);
el("res-refresh").addEventListener("click", loadResources);
el("met-refresh").addEventListener("click", loadMetrics);

el("met-filter").addEventListener("input", () => {
  metricFilter = el("met-filter").value;
  loadMetrics();
});

el("var-refresh").addEventListener("click", loadVariables);
for (const id of ["var-key", "var-value"]) {
  el(id).addEventListener("input", () => {
    varUserTyped = true;
    renderVariables();
  });
}

el("prof-explain").addEventListener("click", async () => {
  const sql = el("prof-sql").value.trim();
  if (!sql) {
    el("prof-plan-status").textContent = "There is no statement to explain.";
    return;
  }
  el("prof-plan-of").textContent = "";
  await runExplain(sql, false, "prof-plan", "prof-plan-status");
});

// --- Compactions and tablet health ---

let compactions = null;

async function loadCompactions() {
  el("comp-status").textContent = "Reading the compaction state...";
  try {
    compactions = await window.compactionsState();
  } catch (err) {
    el("comp-status").textContent = String(err);
    return;
  }
  renderCompactions();
}

function renderCompactions() {
  if (!compactions) return;
  el("comp-status").textContent = compactions.error || "";

  const h = compactions.health || {};
  const bad = (h.unhealthy || 0) + (h.inconsistent || 0) + (h.errorState || 0);
  const p = el("comp-health");
  p.replaceChildren();
  const text = (h.unhealthy || 0) + " unhealthy, " + (h.inconsistent || 0) +
    " inconsistent, " + (h.cloning || 0) + " cloning, " + (h.errorState || 0) +
    " in error";
  if (bad > 0) {
    p.className = "warn";
    p.textContent = text;
  } else {
    p.className = "hint";
    p.textContent = "No tablet is in trouble: " + text + ".";
  }

  el("comp-running").replaceChildren(dataTable(
    ["Partition", "Txn", "Started", "Committed", "Finished", "Error", "Profile"],
    (compactions.compactions || []).map((c) => [
      c.partition, c.txnId, c.startTime, c.commitTime, c.finishTime,
      c.error, c.profile,
    ])));

  el("comp-backends").replaceChildren(dataTable(
    ["Backend", "Candidates", "Base", "Cumulative", "Latest score",
      "Max candidate score", "Manual", "Manual candidates"],
    (compactions.backends || []).map((b) => [
      b.beId, b.candidates, b.baseConcurrency, b.cumulativeConcurrency,
      b.latestScore, b.candidateMaxScore, b.manualConcurrency,
      b.manualCandidates,
    ])));

  el("comp-txns").replaceChildren(dataTable(
    ["Backend", "Load", "Txn", "Partition", "Tablet", "Created", "Committed",
      "Rows", "Size"],
    (compactions.txns || []).map((t) => [
      t.beId, t.loadId, t.txnId, t.partitionId, t.tabletId, t.createTime,
      t.commitTime, t.numRow, bytes(t.dataSize),
    ])));
}

// --- Query profile ---

let profile = null;
let auditLoaded = false;
// The query the user chose, and the rows the lists are filtered by.
let chosen = null;
const profFilters = {
  summaryStatement: "",
  listQueryId: "",
  listStatement: "",
};

async function loadProfile(withAudit) {
  el("prof-status").textContent = "Reading the profiles...";
  try {
    profile = await window.profileState(withAudit);
  } catch (err) {
    el("prof-status").textContent = String(err);
    return;
  }
  if (withAudit) {
    auditLoaded = true;
  }
  renderProfile();
}

function renderProfile() {
  if (!profile) return;
  const bits = [];
  if (profile.error) {
    bits.push(profile.error);
  }
  if (profile.historyError) {
    bits.push("the audit log could not be read: " + profile.historyError);
  } else if (profile.historyCount) {
    let line = profile.historyCount + " queries in the audit log";
    const kept = (profile.summary || []).length;
    if (profile.summaryTotal > kept) {
      line += " · the heaviest " + kept + " of " + profile.summaryTotal +
        " statements";
    }
    bits.push(line);
  } else if (!auditLoaded) {
    bits.push("the audit log has not been read yet");
  }
  el("prof-status").textContent = bits.join(" · ");

  // The filters are regular expressions: a query id is a run of hex
  // digits, and a statement is searched for the shape of it, not for one
  // word of it.
  const summaryFilter = regexpFilter(profFilters.summaryStatement);
  el("prof-summary-filter-status").textContent = summaryFilter.error || "";
  const summary = (profile.summary || []).filter((s) =>
    matches(summaryFilter, s.statement));
  el("prof-summary").replaceChildren(dataTable(
    ["Statement", "Calls", "CPU sec", "Total ms", "Max ms", "Errors", "User",
      "Database", "Last run"],
    summary.map((s) => [
      { text: s.statement, cls: "stmt", title: s.statement },
      { text: s.calls, cls: "num" },
      { text: s.cpuSec.toFixed(2), cls: "num" },
      { text: s.totalMs, cls: "num" },
      { text: s.maxMs, cls: "num" },
      { text: s.errors, cls: "num" },
      { text: s.user, cls: "plain" },
      { text: s.db, cls: "plain" },
      { text: s.last, cls: "plain" },
    ]),
    // The row is a list of cells; the statement it stands for is the
    // summary entry at the same index, which is what the plan needs.
    (_cells, i) => chooseQuery({
      key: summaryKey(summary[i]),
      statement: summary[i].statement,
      queryId: "",
    }, true),
    (_cells, i) => summaryKey(summary[i]),
    ["40%", "6%", "6%", "7%", "7%", "6%", "5%", "10%", "13%"]));

  const idFilter = regexpFilter(profFilters.listQueryId);
  const stmtFilter = regexpFilter(profFilters.listStatement);
  const filterError = idFilter.error || stmtFilter.error;
  el("prof-list-filter-status").textContent = filterError || "";
  const list = (profile.profiles || []).filter((p) =>
    matches(idFilter, p.queryId) && matches(stmtFilter, p.statement));
  el("prof-list").replaceChildren(dataTable(
    ["Query id", "Started", "Time", "State", "Statement"],
    list.map((p) => [
      { text: p.queryId, cls: "plain mono" },
      { text: p.startTime, cls: "plain" },
      { text: p.time, cls: "num" },
      { text: p.state, cls: "plain" },
      { text: p.statement, cls: "stmt", title: p.statement },
    ]),
    (_cells, i) => chooseQuery({
      key: "p:" + list[i].queryId,
      statement: list[i].statement,
      queryId: list[i].queryId,
    }, false),
    (_cells, i) => "p:" + list[i].queryId,
    ["22%", "14%", "6%", "10%", "48%"]));
}

// summaryKey names one statement of the ranking: the digest is its
// identity, and the text is the fallback when the frontend wrote no digest.
function summaryKey(s) {
  return "s:" + (s.digest || s.statement);
}

// regexpFilter compiles a search pattern. An empty pattern matches
// everything; a pattern that does not compile is an error, and the screen
// says so rather than hiding the rows.
function regexpFilter(text) {
  if (!text) return { re: null, error: "" };
  try {
    return { re: new RegExp(text, "i"), error: "" };
  } catch (err) {
    return { re: null, error: "the pattern is not a regular expression: " + String(err.message || err) };
  }
}

// matches reports whether a value passes a filter.
function matches(filter, value) {
  if (!filter.re) return true;
  return filter.re.test(value === null || value === undefined ? "" : String(value));
}

// chooseQuery takes the row the user picked. A row of the profile list is a
// query the frontend recorded a profile of, so it opens that profile in a
// window of its own. A row of the ranking has only a statement: it is
// explained here, and only when the caller asks for it.
async function chooseQuery(pick, explain) {
  chosen = pick;
  el("prof-sql").value = pick.statement || "";
  el("prof-plan").replaceChildren();
  el("prof-plan-of").textContent = "";
  el("prof-plan-status").textContent = "";
  renderProfile();
  if (pick.queryId) {
    try {
      await window.openProfile(pick.queryId);
    } catch (err) {
      el("prof-status").textContent = String(err);
    }
    return;
  }
  if (explain) {
    runExplain(pick.statement, true, "prof-plan", "prof-plan-status");
  }
}

// runExplain explains a statement and draws the plan.
//
// The statement runs again: EXPLAIN ANALYZE is not a plan of a query that
// never happened. So a statement that writes is not run without being asked
// for, however it got here.
async function runExplain(sql, automatic, outId, statusId) {
  const out = el(outId);
  const status = el(statusId);
  if (!sql || !sql.trim()) return;
  if (automatic && !isReadStatement(sql)) {
    status.textContent = "This statement writes: explain analyze runs it again, " +
      "and this time for real. Press Explain analyze when you mean it.";
    return;
  }
  status.textContent = "Running...";
  out.replaceChildren();
  try {
    const plan = await window.explainAnalyze(sql);
    renderPlan(plan, out);
    status.textContent = "Done.";
  } catch (err) {
    status.textContent = String(err);
  }
}

// isReadStatement reports whether a statement only reads.
function isReadStatement(sql) {
  const word = (sql.trim().split(/[\s(]+/)[0] || "").toUpperCase();
  return ["SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "VALUES"]
    .includes(word);
}

// renderPlan draws the plan of a query.
function renderPlan(plan, out) {
  out.replaceChildren();
  if (!plan) return;
  if (plan.error) {
    const line = document.createElement("div");
    line.className = "tree-error";
    line.textContent = plan.error;
    out.append(line);
    return;
  }
  if ((plan.summary || []).length) {
    const summary = document.createElement("div");
    summary.className = "tree-summary";
    for (const field of plan.summary) {
      summary.append(fieldEl(field));
    }
    out.append(summary);
  }
  if ((plan.topTime || []).length) {
    const head = document.createElement("div");
    head.className = "tree-subhead";
    head.textContent = "The nodes that cost the most";
    out.append(head);
    const list = document.createElement("ul");
    list.className = "tree-list";
    for (const line of plan.topTime) {
      const item = document.createElement("li");
      item.textContent = line;
      list.append(item);
    }
    out.append(list);
  }
  for (const fragment of plan.fragments || []) {
    out.append(nodeEl(fragment));
  }
}

// nodeEl draws one node of a plan or of a profile, and what is under it.
function nodeEl(node) {
  const div = document.createElement("div");
  div.className = "tree-node";
  const head = document.createElement("div");
  head.className = "tree-head";
  const name = document.createElement("span");
  name.className = "tree-name";
  name.textContent = node.name;
  head.append(name);
  if (node.value) {
    const value = document.createElement("span");
    value.className = "tree-value";
    value.textContent = node.value;
    head.append(value);
  }
  div.append(head);
  const children = node.children || [];
  if (children.length) {
    const body = document.createElement("div");
    body.className = "tree-body";
    for (const child of children) {
      if ((child.children || []).length) {
        body.append(nodeEl(child));
      } else {
        body.append(fieldEl(child));
      }
    }
    div.append(body);
  }
  return div;
}

// fieldEl draws one measurement: a label and its value.
function fieldEl(node) {
  const line = document.createElement("div");
  line.className = "tree-line";
  const label = document.createElement("span");
  label.className = "tree-label";
  label.textContent = node.name;
  line.append(label);
  if (node.value) {
    const value = document.createElement("span");
    value.className = "tree-value";
    value.textContent = node.value;
    line.append(value);
  }
  return line;
}

// --- Loads ---

let loads = null;

async function loadLoads() {
  el("load-status").textContent = "Reading the import history...";
  try {
    loads = await window.loadsState();
  } catch (err) {
    el("load-status").textContent = String(err);
    return;
  }
  renderLoads();
}

function renderLoads() {
  if (!loads) return;
  el("load-status").textContent = loads.error || "";

  el("load-list").replaceChildren(dataTable(
    ["Id", "Label", "Database", "Table", "User", "State", "Type", "Scanned",
      "Loaded", "Finished", "Error"],
    (loads.loads || []).map((l) => [
      l.id, l.label, l.dbName, l.tableName, l.user, l.state, l.type,
      l.scanRows, l.sinkRows, l.finished, l.failed ? "yes" : "", l.errorMsg,
    ].slice(0, 11))));

  el("load-routine").replaceChildren(dataTable(
    ["Name", "Id", "Database", "Statistic"],
    (loads.routineLoads || []).map((r) => [
      r.name, r.id, r.dbName, r.statistic,
    ])));

  el("load-stream").replaceChildren(dataTable(
    ["Label", "Id", "Database", "Table", "State"],
    (loads.streamLoads || []).map((l) => [
      l.label, l.id, l.dbName, l.tableName, l.state,
    ])));
}

// --- Resources ---

let resources = null;

async function loadResources() {
  el("res-status").textContent = "Reading the limits...";
  try {
    resources = await window.resourcesState();
  } catch (err) {
    el("res-status").textContent = String(err);
    return;
  }
  renderResources();
}

function renderResources() {
  if (!resources) return;
  el("res-status").textContent = resources.error || "";

  el("res-warehouses").replaceChildren(dataTable(
    ["Name", "State", "Nodes", "Clusters", "Max clusters", "Running SQL",
      "Queued SQL", "Comment"],
    (resources.warehouses || []).map((w) => [
      w.name, w.state, w.nodeCount, w.currentClusterCount, w.maxClusterCount,
      w.runningSql, w.queuedSql, w.comment,
    ])));

  el("res-groups").replaceChildren(dataTable(
    ["Name", "Id", "CPU weight", "Exclusive cores", "Memory", "Big query CPU",
      "Big query rows", "Big query memory", "Concurrency", "Spill", "Classifiers"],
    (resources.groups || []).map((g) => [
      g.name, g.id, g.cpuWeight, g.exclusiveCpuCores, g.memLimit,
      g.bigQueryCpuSecondLimit, g.bigQueryScanRowsLimit, g.bigQueryMemLimit,
      g.concurrencyLimit, g.spillMemLimitThreshold, g.classifiers,
    ])));

  el("res-config").replaceChildren(dataTable(
    ["Name", "Type", "Key", "Value"],
    (resources.resources || []).map((r) => [r.name, r.type, r.key, r.value])));
}

// --- Metrics explorer ---

let metrics = null;
let metricFilter = "";

async function loadMetrics() {
  el("met-status").textContent = "Reading the metrics...";
  try {
    metrics = await window.metricsState(metricFilter);
  } catch (err) {
    el("met-status").textContent = String(err);
    return;
  }
  renderMetrics();
}

function renderMetrics() {
  if (!metrics) return;
  el("met-status").textContent = metrics.error ||
    (metrics.rows.length + " of " + metrics.total + " metrics");

  el("met-list").replaceChildren(dataTable(
    ["Scope", "Node", "Name", "Labels", "Value"],
    (metrics.rows || []).map((m) => [m.scope, m.owner, m.name, m.labels, m.value])));
}

// --- Frontend variables ---

// The configuration the frontend runs with, read whole: 726 settings on a
// stock 4.0 frontend, which is small enough to filter on the page and keep
// the filters immediate.
let variables = null;

// A filter counts only when the user typed it. WebKit fills a search field on
// its own now and then as the screen is shown, and a value nobody typed must
// never hide rows. Entering the screen starts unfiltered; Refresh keeps what
// the user has typed.
let varUserTyped = false;

async function loadVariables() {
  el("var-status").textContent = "Reading the frontend's configuration...";
  try {
    variables = await window.variablesState();
  } catch (err) {
    el("var-status").textContent = String(err);
    return;
  }
  renderVariables();
}

function renderVariables() {
  if (!variables) return;
  if (!varUserTyped) {
    el("var-key").value = "";
    el("var-value").value = "";
  }
  const key = el("var-key").value.trim().toLowerCase();
  const value = el("var-value").value.trim().toLowerCase();
  const all = variables.rows || [];
  const rows = all.filter((v) =>
    (!key || v.key.toLowerCase().includes(key)) &&
    (!value || v.value.toLowerCase().includes(value)));

  if (variables.error) {
    el("var-status").textContent = variables.error;
  } else if (rows.length === all.length) {
    el("var-status").textContent = all.length + " variables · " +
      variables.mutableCount + " can be changed at run time";
  } else {
    el("var-status").textContent = rows.length + " of " + all.length +
      " variables";
  }

  el("var-list").replaceChildren(dataTable(
    ["Key", "Value", "Type", "Mutable", "Comment"],
    rows.map((v) => [v.key, v.value, v.type, v.mutable ? "yes" : "no", v.comment])));
}

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
  // The ?screen= override opens a screen without a click; the app itself
  // always lands on the dashboard, where the refresh timer does the loading.
  if (screen !== "dashboard") goTo(screen);
})();
