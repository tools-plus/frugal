"use strict";
// Admin-only user management modal: list users, add a user (with a temporary
// password they must change at first login), change a user's role, reset a
// password, or delete a user. All actions call the admin-gated /api/users*
// endpoints; the server enforces the rules (e.g. last-admin protection), and
// this UI just surfaces the errors it returns.

let root = null;
let currentUser = "";

function build() {
  if (root) return root;
  root = document.createElement("div");
  root.id = "users";
  root.innerHTML =
    '<div class="box">' +
      '<div class="ubar"><span class="utitle">Users</span><button class="uclose" aria-label="Close">✕</button></div>' +
      '<div class="uerr" id="uErr"></div>' +
      '<div class="ulist" id="uList"></div>' +
      '<form class="uadd" id="uAdd">' +
        '<input id="uName" placeholder="new username" autocomplete="off">' +
        '<input id="uPass" type="password" placeholder="temp password" autocomplete="new-password">' +
        '<select id="uRole"><option value="viewer">viewer</option><option value="admin">admin</option></select>' +
        '<button type="submit">Add user</button>' +
      '</form>' +
    '</div>';
  document.body.appendChild(root);
  root.querySelector(".uclose").onclick = close;
  root.addEventListener("mousedown", e => { if (e.target === root) close(); });
  root.querySelector("#uAdd").addEventListener("submit", addUser);
  return root;
}

export async function openUsers(me) {
  currentUser = me || "";
  build();
  root.classList.add("open");
  document.addEventListener("keydown", onKey);
  await refresh();
}

function close() {
  if (!root) return;
  root.classList.remove("open");
  document.removeEventListener("keydown", onKey);
}
function onKey(e) { if (e.key === "Escape") close(); }

function err(msg) { document.getElementById("uErr").textContent = msg || ""; }

async function api(method, path, body) {
  const opts = { method };
  if (body !== undefined) { opts.headers = { "Content-Type": "application/json" }; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  let d = {};
  try { d = await r.json(); } catch {}
  if (!r.ok) throw new Error((d && d.error) || r.statusText || "request failed");
  return d;
}

async function refresh() {
  err("");
  let users = [];
  try { users = await fetch("/api/users").then(r => r.json()) || []; }
  catch { err("could not load users"); return; }
  const list = document.getElementById("uList");
  list.innerHTML = "";
  for (const u of users) {
    const row = document.createElement("div");
    row.className = "urow";
    const self = u.username === currentUser;
    row.innerHTML =
      `<span class="uname">${u.username}${self ? ' <span class="uyou">(you)</span>' : ""}` +
      `${u.must_change ? ' <span class="upend">must change pw</span>' : ""}</span>`;
    // role selector
    const roleSel = document.createElement("select");
    for (const rr of ["viewer", "admin"]) {
      const o = document.createElement("option");
      o.value = rr; o.textContent = rr; if (rr === u.role) o.selected = true;
      roleSel.appendChild(o);
    }
    roleSel.onchange = async () => {
      try { await api("POST", `/api/users/${encodeURIComponent(u.username)}/role`, { role: roleSel.value }); await refresh(); }
      catch (e) { err(e.message); await refresh(); }
    };
    // actions
    const resetBtn = document.createElement("button");
    resetBtn.textContent = "reset pw";
    resetBtn.onclick = async () => {
      const np = prompt(`New temporary password for "${u.username}" (min 6 chars):`);
      if (np == null) return;
      try { await api("POST", `/api/users/${encodeURIComponent(u.username)}/password`, { new_password: np }); err(""); await refresh(); }
      catch (e) { err(e.message); }
    };
    const delBtn = document.createElement("button");
    delBtn.textContent = "delete";
    delBtn.className = "udel";
    delBtn.disabled = self;
    delBtn.onclick = async () => {
      if (!confirm(`Delete user "${u.username}"?`)) return;
      try { await api("DELETE", `/api/users/${encodeURIComponent(u.username)}`); err(""); await refresh(); }
      catch (e) { err(e.message); }
    };
    row.append(roleSel, resetBtn, delBtn);
    list.appendChild(row);
  }
}

async function addUser(e) {
  e.preventDefault();
  err("");
  const name = document.getElementById("uName").value.trim();
  const pass = document.getElementById("uPass").value;
  const role = document.getElementById("uRole").value;
  if (!name) { err("username required"); return; }
  if (pass.length < 6) { err("password must be at least 6 characters"); return; }
  try {
    await api("POST", "/api/users", { username: name, password: pass, role });
    document.getElementById("uName").value = "";
    document.getElementById("uPass").value = "";
    await refresh();
  } catch (e2) { err(e2.message); }
}
