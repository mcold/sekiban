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

async function loadProfile() {
  el("prof-window-status").textContent = "Reading the recorded profile...";
  let doc;
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

el("prof-window-refresh").addEventListener("click", loadProfile);
loadProfile();
