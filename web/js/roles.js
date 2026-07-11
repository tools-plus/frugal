"use strict";
// Admin-only Roles modal: define named roles that grant view access to a chosen
// set of services, then assign them to users (in the Users modal). The built-in
// "admin" (manage + everything) and "viewer" (all services) roles are shown but
// not editable. Enforcement is server-side; this is just the editor.

import { SVCMETA } from "./state.js";

// The service keys a role can grant, in rail order (matches server serviceOf()).
const SERVICES = ["EKS", "Hosts", "EC2", "ALB", "NLB", "RDS", "DocDB", "Valkey", "MQ", "OpenSearch", "S3", "Insights"];
const label = k => (SVCMETA[k] ? SVCMETA[k].title : k);

let root = null;
let editing = null;   // role name being edited, or null = create mode

function build() {
  if (root) return root;
  root = document.createElement("div");
  root.id = "roles";
  root.innerHTML =
    '<div class="box">' +
      '<div class="rbar"><span class="rtitle">Roles</span><button class="rclose" aria-label="Close">✕</button></div>' +
      '<div class="rerr" id="rErr"></div>' +
      '<div class="rlist" id="rList"></div>' +
      '<div class="reditor">' +
        '<div class="rehead" id="reHead">New role</div>' +
        '<input id="reName" placeholder="role name (e.g. db-team)" autocomplete="off">' +
        '<div class="resvcs" id="reSvcs"></div>' +
        '<div class="reactions">' +
          '<button id="reCancel" hidden>cancel</button>' +
          '<button id="reSave" class="primary">Create role</button>' +
        '</div>' +
      '</div>' +
    '</div>';
  document.body.appendChild(root);
  // service checkboxes
  const box = root.querySelector("#reSvcs");
  for (const k of SERVICES) {
    const lbl = document.createElement("label");
    lbl.innerHTML = `<input type="checkbox" value="${k}"> ${label(k)}`;
    box.appendChild(lbl);
  }
  root.querySelector(".rclose").onclick = close;
  root.addEventListener("mousedown", e => { if (e.target === root) close(); });
  root.querySelector("#reSave").onclick = save;
  root.querySelector("#reCancel").onclick = () => setEditor(null);
  return root;
}

export async function openRoles() {
  build();
  root.classList.add("open");
  document.addEventListener("keydown", onKey);
  setEditor(null);
  await refresh();
}

function close() {
  if (!root) return;
  root.classList.remove("open");
  document.removeEventListener("keydown", onKey);
}
function onKey(e) { if (e.key === "Escape") close(); }
function err(msg) { document.getElementById("rErr").textContent = msg || ""; }

function checkedServices() {
  return [...root.querySelectorAll("#reSvcs input:checked")].map(c => c.value);
}
function setChecked(services) {
  const all = services.includes("*");
  root.querySelectorAll("#reSvcs input").forEach(c => { c.checked = all || services.includes(c.value); });
}

// setEditor(null) = create mode; setEditor(role) = edit an existing role.
function setEditor(role) {
  editing = role ? role.name : null;
  const name = document.getElementById("reName");
  document.getElementById("reHead").textContent = role ? `Edit role: ${role.name}` : "New role";
  name.value = role ? role.name : "";
  name.disabled = !!role;
  setChecked(role ? role.services : []);
  document.getElementById("reSave").textContent = role ? "Save changes" : "Create role";
  document.getElementById("reCancel").hidden = !role;
}

async function api(method, path, body) {
  const r = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
  let d = {}; try { d = await r.json(); } catch {}
  if (!r.ok) throw new Error((d && d.error) || r.statusText || "request failed");
  return d;
}

async function refresh() {
  err("");
  let roles = [];
  try { roles = await fetch("/api/roles").then(r => r.json()) || []; }
  catch { err("could not load roles"); return; }
  const list = document.getElementById("rList");
  list.innerHTML = "";
  for (const role of roles) {
    const builtin = role.is_admin || role.name === "viewer";
    const summary = role.services.includes("*") ? "all services" : (role.services.map(label).join(", ") || "—");
    const row = document.createElement("div");
    row.className = "rrow";
    row.innerHTML =
      `<span class="rname">${role.name}${role.is_admin ? ' <span class="radmin">admin</span>' : ""}</span>` +
      `<span class="rsvcs">${summary}</span>`;
    if (!builtin) {
      const edit = document.createElement("button");
      edit.textContent = "edit";
      edit.onclick = () => { setEditor(role); root.querySelector("#reName").scrollIntoView({ block: "nearest" }); };
      const del = document.createElement("button");
      del.textContent = "delete"; del.className = "rdel";
      del.onclick = async () => {
        if (!confirm(`Delete role "${role.name}"?`)) return;
        try { await api("DELETE", `/api/roles/${encodeURIComponent(role.name)}`); if (editing === role.name) setEditor(null); await refresh(); }
        catch (e) { err(e.message); }
      };
      row.append(edit, del);
    } else {
      const tag = document.createElement("span");
      tag.className = "rbuiltin"; tag.textContent = "built-in";
      row.append(tag);
    }
    list.appendChild(row);
  }
}

async function save() {
  err("");
  const name = document.getElementById("reName").value.trim();
  const services = checkedServices();
  if (!editing && !name) { err("role name required"); return; }
  if (!services.length) { err("choose at least one service"); return; }
  try {
    if (editing) await api("POST", `/api/roles/${encodeURIComponent(editing)}`, { services });
    else await api("POST", "/api/roles", { name, services });
    setEditor(null);
    await refresh();
  } catch (e) { err(e.message); }
}
