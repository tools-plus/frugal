"use strict";
// Self-service "change password" modal, opened from the Profile menu. Uses the
// session-scoped /api/change-password endpoint (the server identifies the user
// from the cookie). Closes on Cancel, Esc, or backdrop click.

let root = null;

function build() {
  if (root) return root;
  root = document.createElement("div");
  root.id = "account";
  root.innerHTML =
    '<div class="box">' +
      '<div class="abar"><span class="atitle">Change password</span><button class="aclose" aria-label="Close">✕</button></div>' +
      '<input id="aNew" type="password" placeholder="new password" autocomplete="new-password">' +
      '<input id="aConf" type="password" placeholder="confirm new password" autocomplete="new-password">' +
      '<div class="amsg" id="aMsg"></div>' +
      '<div class="aactions">' +
        '<button id="aCancel">cancel</button>' +
        '<button id="aSave" class="primary">Update password</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(root);
  root.querySelector(".aclose").onclick = close;
  root.querySelector("#aCancel").onclick = close;
  root.addEventListener("mousedown", e => { if (e.target === root) close(); });
  root.querySelector("#aSave").onclick = save;
  root.addEventListener("submit", e => e.preventDefault());
  return root;
}

export function openChangePassword() {
  build();
  root.querySelector("#aNew").value = "";
  root.querySelector("#aConf").value = "";
  msg("", false);
  root.classList.add("open");
  document.addEventListener("keydown", onKey);
  root.querySelector("#aNew").focus();
}

function close() {
  if (!root) return;
  root.classList.remove("open");
  document.removeEventListener("keydown", onKey);
}
function onKey(e) { if (e.key === "Escape") close(); }
function msg(text, ok) {
  const el = root.querySelector("#aMsg");
  el.textContent = text || "";
  el.style.color = ok ? "var(--ok)" : "var(--bad)";
}

async function save() {
  const np = root.querySelector("#aNew").value, conf = root.querySelector("#aConf").value;
  if (np.length < 6) { msg("password must be at least 6 characters", false); return; }
  if (np !== conf) { msg("passwords do not match", false); return; }
  try {
    const r = await fetch("/api/change-password", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ new_password: np }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok || !d.ok) { msg((d && d.error) || "could not change password", false); return; }
  } catch { msg("network error", false); return; }
  msg("password updated", true);
  setTimeout(close, 900);
}
