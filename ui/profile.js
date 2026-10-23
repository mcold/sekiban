"use strict";

// The profile window: the profile the frontend recorded of one query, drawn
// as the tree it is.
//
// The window knows which query it shows. It does not know which server that
// is: the profile is read over the connection attached right now, so a
// window left open never reads a server the user has left.

const el = (id) => document.getElementById(id);

window.addEventListener("error", (ev) => {
  window.logError?.(String(ev.message || ev.error));
});
window.addEventListener("unhandledrejection", (ev) => {
  window.logError?.(String(ev.reason));
});

// doc is the profile as the frontend wrote it, kept whole: it is what the
// window draws and what its Copy button hands over.
let doc = null;

async function loadProfile() {
  el("prof-window-status").textContent = "Reading the recorded profile...";
  try {
    doc = await window.profileDetail();
  } catch (err) {
    el("prof-window-status").textContent = String(err);
    return;
  }
  el("prof-title").textContent = "Profile of query " + (doc.queryId || "");
  el("prof-window-status").textContent = doc.error || "";
  const box = el("prof-window-tree");
  box.replaceChildren();
  for (const node of doc.nodes || []) {
    box.append(nodeEl(node));
  }
}

// copyText puts text on the clipboard. The WebView has no permission API to
// ask, so the clipboard API is tried and the hidden-textarea road is the
// fallback; whoever calls this says so when both fail.
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (err) {
    // The custom scheme may count as insecure; fall through.
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.top = "-1000px";
  document.body.append(area);
  area.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch (err) {
    ok = false;
  }
  area.remove();
  return ok;
}

// nodeEl draws a section of the profile and everything under it.
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

el("prof-window-copy").addEventListener("click", async () => {
  const status = el("prof-window-status");
  const text = doc && doc.raw;
  if (!text) {
    status.textContent = "There is no profile to copy.";
    return;
  }
  if (await copyText(text)) {
    status.textContent = "The profile is on the clipboard.";
  } else {
    status.textContent = "The clipboard refused the profile.";
  }
});

el("prof-window-refresh").addEventListener("click", loadProfile);
loadProfile();
