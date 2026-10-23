// Mock of the Go bind layer, so the interface opens in a plain browser with
// no database behind it: for screenshots, and for working on the UI offline.
// Loaded before app.js by tools/uimock/serve.sh.
//
// The data comes from recorded/*.json, which are REAL captures taken with
// `sekiban -json dashboard` against a live StarRocks under load. Screenshots
// must not invent numbers: a chart drawn from a smooth synthetic curve looks
// nothing like a cluster under load, and publishing it as if it were a
// measurement would be a lie. Re-record with tools/record_demo.sh.
"use strict";

const PARAMS = new URLSearchParams(location.search);
const THEME = PARAMS.get("theme");
if (THEME) document.documentElement.style.colorScheme = THEME;
// ?screen=connections is read by app.js itself, so a headless capture reaches
// any screen without clicking and the real app still always opens on the
// dashboard.

async function recorded(name) {
  const r = await fetch("recorded/" + name + ".json");
  if (!r.ok) throw new Error("missing recorded/" + name + ".json");
  return r.json();
}

// The recorded load chart carries the timestamps of the moment it was taken.
// Shifting them to now keeps the axis readable without touching a value.
function shiftToNow(buckets) {
  if (!buckets || buckets.length === 0) return buckets;
  const offset = Date.now() - buckets[buckets.length - 1].t;
  return buckets.map((b) => ({ t: b.t + offset, v: b.v }));
}

// The connections screen is the one place the mock has to keep state: connect,
// disconnect and promote say nothing unless the list answers back. A second
// entry is what makes the promote and the "one open at a time" rule visible;
// no captured screenshot shows this screen, so nothing published depends on it.
const MOCK = {
  active: 0,
  killed: 0,
  connections: [
    { title: "analytics", dsn: "root:...@tcp(fe.internal:9030)/analytics", feHttp: "http://fe.internal:8030" },
    { title: "staging", dsn: "root:...@tcp(stage.internal:9030)/app", feHttp: "http://stage.internal:8030" },
  ],
};

window.configState = async () => ({
  path: "~/.config/sekiban/init.filo",
  exists: true,
  error: "",
  active: MOCK.active,
  connections: MOCK.connections,
});

window.dashboardState = async (_win, sliceBy, topBy) => {
  const d = await recorded("dashboard");
  d.buckets = shiftToNow(d.buckets);
  d.conns = shiftToNow(d.conns);
  d.sliceBy = sliceBy || d.sliceBy;
  d.topBy = topBy || d.topBy;
  return d;
};

window.sessionList = async () => recorded("sessions");
window.clusterState = async () => recorded("cluster");
window.compactionsState = async () => recorded("compactions");
window.loadsState = async () => recorded("loads");
window.resourcesState = async () => recorded("resources");
window.metricsState = async (filter) => {
  const m = await recorded("metrics");
  if (!filter) return m;
  const needle = filter.toLowerCase();
  m.rows = m.rows.filter((r) => r.name.toLowerCase().includes(needle));
  return m;
};
window.variablesState = async () => recorded("variables");
window.browserTree = async () => recorded("browser");
window.profileState = async () => recorded("profiles");
window.explainAnalyze = async () => recorded("explain");
window.openProfile = async () => ({});
window.killSession = async (id) => {
  MOCK.killed = id;
  return {};
};

window.connectTo = async (index) => {
  MOCK.active = index;
  return window.configState();
};

window.disconnect = async () => {
  MOCK.active = -1;
  return window.configState();
};

window.makeDefault = async (index) => {
  const c = MOCK.connections.splice(index, 1)[0];
  MOCK.connections.unshift(c);
  MOCK.active = 0;
  return window.configState();
};

window.addConnection = async (dsn, title) => {
  MOCK.connections.push({ title: title || "connection", dsn: dsn, feHttp: "" });
  MOCK.active = MOCK.connections.length - 1;
  return window.configState();
};

window.updateConnection = async (index, dsn, title) => {
  MOCK.connections[index] = { title: title || "connection", dsn: dsn, feHttp: "" };
  return window.configState();
};

window.deleteConnection = async (index) => {
  MOCK.connections.splice(index, 1);
  if (MOCK.active >= MOCK.connections.length) MOCK.active = 0;
  return window.configState();
};

window.connectionDSN = async (index) => MOCK.connections[index].dsn;

window.testConnection = async (dsn) => {
  if (!dsn) return { ok: false, error: "the connection string is required" };
  return { ok: true, version: "4.0.0-dccb665" };
};

window.logError = async (msg) => {
  console.warn("ui error:", msg);
};
