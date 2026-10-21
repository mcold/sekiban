"use strict";

// The object window: one table, view or materialized view of the attached
// database, with its catalog and a console to read and edit it.
//
// The window knows which object it shows. It does not know which database
// that is: every call goes through the connection that is attached right
// now, so a window left open never reads a server the user has left.

const el = (id) => document.getElementById(id);

window.addEventListener("error", (ev) => {
  window.logError?.(String(ev.message || ev.error));
});
window.addEventListener("unhandledrejection", (ev) => {
  window.logError?.(String(ev.reason));
});

let detail = null;
let result = null;
// edits is the set of changed cells, keyed by the row index and then by the
// column index.
let edits = new Map();
// editing is the cell being edited right now, or null.
let editing = null;

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

async function loadDetail() {
  el("obj-status").textContent = "Reading the catalog...";
  try {
    detail = await window.objectDetail();
  } catch (err) {
    el("obj-status").textContent = String(err);
    return;
  }
  el("obj-title").textContent = detail.name;
  el("obj-status").textContent = detail.error || "";
  renderProperties();
  renderGrid();
}

// --- Properties ---

function renderProperties() {
  const box = el("obj-properties");
  box.replaceChildren();
  if (!detail || detail.error) return;

  if (detail.type) {
    const kind = document.createElement("p");
    kind.className = "hint";
    kind.textContent = [detail.type, detail.model].filter(Boolean).join(" · ");
    box.append(kind);
  }

  if (detail.ddl) {
    box.append(heading("Definition"));
    const pre = document.createElement("pre");
    pre.className = "ddl";
    pre.textContent = detail.ddl;
    box.append(pre);
  }

  if (detail.columns && detail.columns.length) {
    box.append(heading("Columns"));
    box.append(table(
      ["Name", "Type", "Null", "Key", "Default", "Comment"],
      detail.columns.map((c) => [
        c.name, c.type, c.nullable ? "yes" : "no", c.key, c.default, c.comment,
      ])));
  }

  if (detail.model && detail.model !== "VIEW") {
    box.append(heading("Storage"));
    const rows = [
      ["Primary key", detail.primaryKey || "(none)"],
      ["Partition key", detail.partitionKey || "(none)"],
      ["Distribution", (detail.distributeKey || "(none)") + " " +
        (detail.distributeType || "") + " " + (detail.distributeBucket || "")],
      ["Sort key", detail.sortKey || "(none)"],
    ];
    if (detail.properties) {
      rows.push(["Properties", detail.properties]);
    }
    box.append(table(["", ""], rows));
  }

  if (detail.partitions && detail.partitions.length) {
    box.append(heading("Partitions"));
    box.append(table(
      ["Name", "Key", "Value", "Distribution", "Buckets", "Replicas", "Rows", "Size"],
      detail.partitions.map((p) => [
        p.name, p.partitionKey || "(none)", p.partitionValue || "(none)",
        p.distributionKey || "(none)", p.buckets, p.replication, p.rows,
        bytes(p.size),
      ])));
  }
}

function heading(text) {
  const h = document.createElement("h3");
  h.textContent = text;
  return h;
}

function table(head, rows) {
  const t = document.createElement("table");
  t.className = "data-table";
  const thead = document.createElement("thead");
  const htr = document.createElement("tr");
  for (const h of head) {
    const th = document.createElement("th");
    th.textContent = h;
    htr.append(th);
  }
  thead.append(htr);
  t.append(thead);
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
  t.append(tbody);
  return t;
}

// --- Data ---

async function runQuery() {
  const sql = el("sql-text").value;
  el("sql-status").textContent = "Running...";
  el("grid-save").hidden = true;
  edits = new Map();
  try {
    result = await window.runQuery(sql);
  } catch (err) {
    el("sql-status").textContent = String(err);
    return;
  }
  if (result.error) {
    el("sql-status").textContent = result.error;
    result = null;
    renderGrid();
    return;
  }
  const bits = [];
  if (result.returnsRows) {
    bits.push(result.rows.length + " rows");
    if (result.truncated) bits.push("truncated");
  } else {
    bits.push(result.affected + " rows changed");
  }
  bits.push(result.elapsedMS.toFixed(1) + " ms");
  el("sql-status").textContent = bits.join(" · ");
  renderGrid();
}

function renderGrid() {
  const box = el("grid");
  box.replaceChildren();
  if (!result) return;

  if (!result.returnsRows) {
    const p = document.createElement("p");
    p.className = "hint";
    p.textContent = "The statement changed " + result.affected + " rows.";
    box.append(p);
    return;
  }

  // The grid is editable only when the statement reads the object this
  // window is showing, and that object is a primary key table: it is the
  // only kind that accepts an UPDATE.
  const editable = editableColumns();
  el("grid-save").hidden = editable === null;
  if (editable === null && detail && detail.name) {
    const p = document.createElement("p");
    p.className = "hint";
    p.textContent = detail.editable
      ? "This result does not read the object this window is showing, so its rows cannot be addressed by a primary key."
      : "This object is not a primary key table, and only a primary key table accepts an update.";
    box.append(p);
  }

  const t = document.createElement("table");
  t.className = "data-table grid";
  const thead = document.createElement("thead");
  const htr = document.createElement("tr");
  for (const c of result.columns) {
    const th = document.createElement("th");
    th.textContent = c.name;
    if (c.type) th.title = c.type;
    htr.append(th);
  }
  thead.append(htr);
  t.append(thead);

  const tbody = document.createElement("tbody");
  result.rows.forEach((row, rowIndex) => {
    const tr = document.createElement("tr");
    row.forEach((cell, colIndex) => {
      const td = document.createElement("td");
      if (cell === null) {
        td.textContent = "NULL";
        td.className = "cell-null";
      } else {
        td.textContent = cell;
      }
      const changed = edits.has(rowIndex) && edits.get(rowIndex).has(colIndex);
      if (changed) td.classList.add("cell-changed");
      const canEdit = editable !== null && editable[colIndex];
      if (canEdit) {
        td.classList.add("cell-editable");
        td.addEventListener("dblclick", () => startEdit(td, rowIndex, colIndex));
      }
      tr.append(td);
    });
    tbody.append(tr);
  });
  t.append(tbody);
  box.append(t);
}

// editableColumns says which columns of the result may be changed, or null
// when the result cannot be edited at all.
//
// The primary key columns are not editable: they address the row, and a
// statement that changes the key of the row it matches changes nothing.
function editableColumns() {
  if (!detail || !detail.editable) return null;
  if (!detail.pkColumns || detail.pkColumns.length === 0) return null;
  if (!result || !result.returnsRows) return null;
  const from = fromTable();
  if (from !== detail.name) return null;
  return result.columns.map((c) => !detail.pkColumns.includes(c.name));
}

// fromTable reads the table a statement reads from.
function fromTable() {
  const sql = (el("sql-text").value || "");
  const m = /\bfrom\s+`?([A-Za-z0-9_]+)`?/i.exec(sql);
  return m ? m[1] : "";
}

function startEdit(td, rowIndex, colIndex) {
  if (editing) return;
  editing = { rowIndex, colIndex, td };

  const current = result.rows[rowIndex][colIndex];
  const input = document.createElement("input");
  input.type = "text";
  input.value = current === null ? "" : current;
  input.className = "cell-input";

  const isNull = current === null;
  const nullBtn = document.createElement("button");
  nullBtn.type = "button";
  nullBtn.className = "cell-null-btn";
  nullBtn.textContent = "NULL";
  nullBtn.title = "Set the cell to NULL";

  const commit = (value, isNullValue) => {
    const rowEdits = edits.get(rowIndex) || new Map();
    rowEdits.set(colIndex, { value: value, isNull: isNullValue });
    edits.set(rowIndex, rowEdits);
    editing = null;
    renderGrid();
  };

  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") {
      commit(input.value, false);
      ev.preventDefault();
    } else if (ev.key === "Escape") {
      editing = null;
      renderGrid();
    }
  });
  input.addEventListener("blur", () => {
    if (editing) commit(input.value, false);
  });
  nullBtn.addEventListener("click", () => commit(null, true));

  const wrap = document.createElement("span");
  wrap.className = "cell-edit";
  wrap.append(input, nullBtn);
  td.replaceChildren(wrap);
  input.focus();
  input.select();
}

// saveChanges shows the exact statement before it runs. The attached
// database may be production.
async function saveChanges() {
  if (edits.size === 0) {
    el("sql-status").textContent = "Nothing to save.";
    return;
  }
  const req = buildEditRequest();
  if (!req) {
    el("sql-status").textContent = "The result has no primary key, so the row cannot be addressed.";
    return;
  }
  let stmt;
  try {
    stmt = await window.gridPreview(req);
  } catch (err) {
    el("sql-status").textContent = String(err);
    return;
  }

  const box = el("grid");
  const confirm = document.createElement("div");
  confirm.className = "confirm";
  const pre = document.createElement("pre");
  pre.className = "ddl";
  pre.textContent = stmt;
  const row = document.createElement("div");
  row.className = "buttons";
  const back = document.createElement("button");
  back.type = "button";
  back.textContent = "Back";
  back.addEventListener("click", () => confirm.remove());
  const go = document.createElement("button");
  go.type = "button";
  go.className = "destructive";
  go.textContent = "Run the UPDATE";
  go.addEventListener("click", () => doSave(req, confirm));
  row.append(back, go);
  confirm.append(pre, row);
  box.prepend(confirm);
}

function buildEditRequest() {
  if (!detail || !detail.editable || !detail.pkColumns || detail.pkColumns.length === 0) {
    return null;
  }
  const request = {
    database: detail.database || "",
    table: detail.name,
    key: [],
    edits: [],
  };
  // The key is the primary key of the first edited row: every row of a
  // result set has the same key columns.
  const firstRow = [...edits.keys()].sort((a, b) => a - b)[0];
  const row = result.rows[firstRow];
  request.key = detail.pkColumns.map((_, i) => (row[i] === null ? "" : String(row[i])));
  for (const [rowIndex, rowEdits] of edits) {
    for (const [colIndex, cell] of rowEdits) {
      request.edits.push({
        column: result.columns[colIndex].name,
        value: cell.isNull ? null : cell.value,
      });
    }
  }
  return request;
}

async function doSave(req, confirm) {
  el("sql-status").textContent = "Saving...";
  try {
    const n = await window.gridSave(req);
    confirm.remove();
    edits = new Map();
    el("sql-status").textContent = n + " rows changed.";
    runQuery();
  } catch (err) {
    el("sql-status").textContent = String(err);
  }
}

// --- Wiring ---

for (const b of document.querySelectorAll(".tab")) {
  b.addEventListener("click", () => {
    for (const other of document.querySelectorAll(".tab")) {
      other.setAttribute("aria-pressed", String(other === b));
    }
    const tab = b.dataset.tab;
    el("obj-properties").hidden = tab !== "properties";
    el("obj-data").hidden = tab !== "data";
  });
}
document.querySelector('.tab[data-tab="properties"]').setAttribute("aria-pressed", "true");

el("obj-refresh").addEventListener("click", loadDetail);
el("sql-run").addEventListener("click", runQuery);
el("sql-cancel").addEventListener("click", async () => {
  try {
    await window.cancelQuery();
    el("sql-status").textContent = "Cancelled.";
  } catch (err) {
    el("sql-status").textContent = String(err);
  }
});
el("grid-save").addEventListener("click", saveChanges);

el("sql-text").addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) {
    runQuery();
    ev.preventDefault();
  }
});

el("sql-run").prepend(icon("play-fill"));
el("obj-refresh").prepend(icon("arrow-clockwise"));

(async () => {
  await loadDetail();
  try {
    el("sql-text").value = await window.initialQuery();
  } catch (err) {
    el("sql-status").textContent = String(err);
  }
})();
