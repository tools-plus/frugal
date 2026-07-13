"use strict";
// Admin-only Settings modal: edits the runtime config (AWS, Kubernetes, native
// targets, retention, ingest token) stored encrypted in the control DB. Saving
// POSTs to /api/settings, which persists and hot-reloads the collector service
// without restarting the web server. Secrets are write-only: they come back
// blank from GET; leaving a secret field empty keeps the stored value.

let root = null;

const KEEP = "leave blank to keep";

// The CloudWatch namespaces awsobs knows how to collect, with friendly labels.
// Rendered as checkboxes (we only support these — not an open-ended list).
const AWS_NAMESPACES = [
  ["AWS/EC2", "EC2"], ["AWS/RDS", "RDS"], ["AWS/DocDB", "DocumentDB"],
  ["AWS/ElastiCache", "ElastiCache / Valkey"], ["AWS/AmazonMQ", "AmazonMQ"],
  ["AWS/ES", "OpenSearch"], ["AWS/S3", "S3"], ["AWS/ApplicationELB", "ALB"],
  ["AWS/NetworkELB", "NLB"], ["AWS/EKS", "EKS control plane"], ["ContainerInsights", "Container Insights"],
];

function h(html) { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstChild; }
function val(id) { return (root.querySelector("#" + id) || {}).value ?? ""; }
function num(id) { const n = parseInt(val(id), 10); return isNaN(n) ? 0 : n; }
function checked(id) { return !!(root.querySelector("#" + id) || {}).checked; }

function build() {
  if (root) return root;
  root = document.createElement("div");
  root.id = "settings";
  root.innerHTML =
    '<div class="box">' +
      '<div class="sbar"><span class="stitle">Settings</span><span class="ssub" id="sSub"></span>' +
        '<button class="sclose" aria-label="Close">✕</button></div>' +
      '<div class="skeywarn" id="sKeyWarn" hidden>⚠ AWSOBS_SECRET_KEY is not set — credentials can\'t be saved until it is.</div>' +
      '<div class="serr" id="sErr"></div>' +
      '<div class="sbody" id="sBody"></div>' +
      '<div class="sactions"><button id="sCancel">cancel</button><button id="sSave" class="primary">Save &amp; apply</button></div>' +
    '</div>';
  document.body.appendChild(root);
  root.querySelector(".sclose").onclick = close;
  root.querySelector("#sCancel").onclick = close;
  root.addEventListener("mousedown", e => { if (e.target === root) close(); });
  root.querySelector("#sSave").onclick = save;
  return root;
}

export async function openSettings() {
  build();
  err("");
  root.classList.add("open");
  document.addEventListener("keydown", onKey);
  let data = {};
  try { data = await fetch("/api/settings").then(r => r.json()); } catch { err("could not load settings"); return; }
  render(data);
}

function close() { if (root) { root.classList.remove("open"); document.removeEventListener("keydown", onKey); } }
function onKey(e) { if (e.key === "Escape") close(); }
function err(m) { root.querySelector("#sErr").textContent = m || ""; }

// ---- repeatable rows ----
let clearAWSKeys = false;

function fieldRow(label, inputHTML) {
  return `<label class="sfield"><span>${label}</span>${inputHTML}</label>`;
}
function listSection(title, addLabel, containerId, addFn) {
  const sec = h(`<div class="ssec"><div class="ssec-h">${title}</div><div class="slist" id="${containerId}"></div>` +
    `<button type="button" class="sadd">+ ${addLabel}</button></div>`);
  sec.querySelector(".sadd").onclick = () => addFn(sec.querySelector("#" + containerId), {});
  return sec;
}
function rmBtn(row) { const b = h('<button type="button" class="srm">remove</button>'); b.onclick = () => row.remove(); return b; }

function render(data) {
  const c = data.config || {};
  root.querySelector("#sKeyWarn").hidden = !!data.has_secret_key;
  root.querySelector("#sSub").textContent = "changes apply live — no restart";
  clearAWSKeys = false;
  const body = root.querySelector("#sBody");
  body.innerHTML = "";

  // ---- AWS ----
  const aws = c.aws || {};
  const awsSec = h('<div class="ssec"><div class="ssec-h">AWS (CloudWatch)</div></div>');
  awsSec.insertAdjacentHTML("beforeend",
    fieldRow("enabled", `<input type="checkbox" id="awsEnabled" ${aws.enabled ? "checked" : ""}>`) +
    fieldRow("region", `<input id="awsRegion" value="${aws.region || ""}" placeholder="us-east-1">`) +
    fieldRow("profile (local only)", `<input id="awsProfile" value="${aws.profile || ""}">`) +
    fieldRow("access key id", `<input id="awsAK" value="${aws.access_key_id || ""}" placeholder="blank = use IRSA/env">`) +
    fieldRow("secret access key", `<input id="awsSK" type="password" placeholder="${data.aws_keys_set ? KEEP : "blank = use IRSA/env"}">`) +
    fieldRow("session token", `<input id="awsST" type="password" placeholder="${KEEP}">`) +
    fieldRow("services", `<div class="snsgrid" id="awsNsGrid"></div>`) +
    fieldRow("poll interval (s)", `<input id="awsPoll" type="number" value="${aws.poll_interval_seconds || 300}">`) +
    fieldRow("discovery interval (min)", `<input id="awsDisc" type="number" value="${aws.discovery_interval_minutes || 10}">`) +
    fieldRow("period (s)", `<input id="awsPeriod" type="number" value="${aws.period_seconds || 60}">`));
  if (data.aws_keys_set) {
    const clr = h('<button type="button" class="sclrkeys">clear keys (use IRSA/env)</button>');
    clr.onclick = () => { clearAWSKeys = true; clr.textContent = "keys will be cleared on save"; clr.disabled = true; };
    awsSec.appendChild(clr);
  }
  body.appendChild(awsSec);
  // namespace checkboxes — only the services we support. Unset (never chosen)
  // defaults to all selected, matching the server's "empty = all defaults".
  const nsGrid = awsSec.querySelector("#awsNsGrid");
  const selected = aws.namespaces && aws.namespaces.length ? aws.namespaces : AWS_NAMESPACES.map(n => n[0]);
  for (const [ns, label] of AWS_NAMESPACES) {
    const cb = h(`<label class="scbx"><input type="checkbox" value="${ns}" ${selected.includes(ns) ? "checked" : ""}> ${label}</label>`);
    nsGrid.appendChild(cb);
  }

  // ---- Kubernetes ----
  const k = c.kubernetes || {};
  const kSec = h('<div class="ssec"><div class="ssec-h">Kubernetes (EKS)</div></div>');
  kSec.insertAdjacentHTML("beforeend",
    fieldRow("enabled", `<input type="checkbox" id="k8sEnabled" ${k.enabled ? "checked" : ""}>`) +
    fieldRow("poll interval (s)", `<input id="k8sPoll" type="number" value="${k.poll_interval_seconds || 15}">`) +
    fieldRow("kubeconfig upload", `<textarea id="k8sKube" rows="4" placeholder="${data.kubeconfig_set ? "kubeconfig stored — paste to replace, blank to keep" : "paste kubeconfig YAML (EKS exec / token / client-cert auth)"}"></textarea>`) +
    fieldRow("kubeconfig contexts (local dev)", `<input id="k8sCtx" value="${(k.contexts || []).join(", ")}" placeholder="ctx-a, ctx-b or *  (runs kubectl proxy)">`));
  const clSec = listSection("clusters (direct API)", "add cluster", "clList", (ct) => {
    const row = h('<div class="srow"></div>');
    row.appendChild(mkInput("name")); row.appendChild(mkInput("api_url"));
    row.appendChild(mkSecret("bearer_token")); row.appendChild(rmBtn(row));
    ct.appendChild(row);
  });
  kSec.appendChild(clSec);
  body.appendChild(kSec);
  (k.clusters || []).forEach(t => addExisting(clSec, t));

  // ---- Native targets ----
  const nat = c.native || {};
  const valSec = listSection("Valkey / ElastiCache", "add valkey", "valList", (ct, t) => addTargetRow(ct, t, ["name", "addr"], true, false));
  const osSec = listSection("OpenSearch", "add opensearch", "osList", (ct, t) => addTargetRow(ct, t, ["name", "url", "username"], true, true));
  const mqSec = listSection("RabbitMQ / AmazonMQ", "add rabbitmq", "mqList", (ct, t) => addTargetRow(ct, t, ["name", "url", "username"], true, true));
  body.append(valSec, osSec, mqSec);
  (nat.valkey || []).forEach(t => addExisting(valSec, t));
  (nat.opensearch || []).forEach(t => addExisting(osSec, t));
  (nat.rabbitmq || []).forEach(t => addExisting(mqSec, t));

  // ---- Retention + ingest ----
  const rSec = h('<div class="ssec"><div class="ssec-h">Retention &amp; ingest</div></div>');
  rSec.insertAdjacentHTML("beforeend",
    fieldRow("in-memory points/series (restart)", `<input id="retPoints" type="number" value="${c.retention_points || 720}">`) +
    fieldRow("db retention (hours)", `<input id="retHours" type="number" value="${c.db_retention_hours || 72}">`) +
    fieldRow("log retention (lines/source)", `<input id="retLogs" type="number" value="${c.log_retention_lines || 2000}">`) +
    fieldRow("ingest token", `<input id="ingTok" type="password" placeholder="${data.ingest_token_set ? KEEP : "shared token for agents"}">`));
  const gen = h('<button type="button" class="sgen">generate token</button>');
  gen.onclick = () => { root.querySelector("#ingTok").type = "text"; root.querySelector("#ingTok").value = randToken(); };
  rSec.appendChild(gen);
  body.appendChild(rSec);
}

function mkInput(k, v) { const i = h(`<input placeholder="${k}" value="${(v ?? "").toString().replace(/"/g, "&quot;")}">`); i.dataset.k = k; return i; }
function mkSecret(k) { const i = h(`<input type="password" placeholder="${KEEP}">`); i.dataset.k = k; i.dataset.secret = "1"; return i; }

function addTargetRow(ct, t, textFields, hasPassword, hasUser) {
  const row = h('<div class="srow"></div>');
  for (const f of textFields) row.appendChild(mkInput(f, t[f]));
  if (hasPassword) row.appendChild(mkSecret("password", t.__hasSecret));
  const tlsWrap = h('<label class="scbx" title="TLS / insecure"><input type="checkbox" data-k="tls"> tls</label>');
  if (t.tls) tlsWrap.querySelector("input").checked = true;
  row.appendChild(tlsWrap);
  row.appendChild(rmBtn(row));
  ct.appendChild(row);
  return row;
}
function addExisting(sec, t) {
  const add = sec.querySelector(".sadd");
  add.onclick();
  const rows = sec.querySelectorAll(".srow");
  const row = rows[rows.length - 1];
  row.querySelectorAll("input[data-k]").forEach(i => {
    if (i.dataset.secret) { i.placeholder = KEEP; return; }
    if (i.type === "checkbox") { i.checked = !!t[i.dataset.k]; return; }
    i.value = t[i.dataset.k] ?? "";
  });
}

function collectTargets(containerId) {
  const out = [];
  root.querySelectorAll("#" + containerId + " .srow").forEach(row => {
    const o = {};
    row.querySelectorAll("input[data-k]").forEach(i => {
      if (i.type === "checkbox") o[i.dataset.k] = i.checked;
      else if (i.dataset.secret) { if (i.value) o[i.dataset.k] = i.value; }
      else o[i.dataset.k] = i.value;
    });
    if (o.name) out.push(o);
  });
  return out;
}

function csv(id) { return val(id).split(",").map(s => s.trim()).filter(Boolean); }

function collect() {
  return {
    clear_aws_keys: clearAWSKeys,
    config: {
      aws: {
        enabled: checked("awsEnabled"), region: val("awsRegion"), profile: val("awsProfile"),
        access_key_id: val("awsAK"), secret_access_key: val("awsSK"), session_token: val("awsST"),
        namespaces: [...root.querySelectorAll("#awsNsGrid input:checked")].map(c => c.value),
        poll_interval_seconds: num("awsPoll"),
        discovery_interval_minutes: num("awsDisc"), period_seconds: num("awsPeriod"),
      },
      kubernetes: {
        enabled: checked("k8sEnabled"), poll_interval_seconds: num("k8sPoll"),
        kubeconfig: val("k8sKube"),
        contexts: csv("k8sCtx"), clusters: collectTargets("clList").map(c => ({
          name: c.name, api_url: c.api_url, bearer_token: c.bearer_token || "",
        })),
      },
      native: {
        valkey: collectTargets("valList"),
        opensearch: collectTargets("osList"),
        rabbitmq: collectTargets("mqList"),
      },
      ingest_token: val("ingTok"),
      retention_points: num("retPoints"),
      db_retention_hours: num("retHours"),
      log_retention_lines: num("retLogs"),
    },
  };
}

function randToken() {
  const a = new Uint8Array(24); crypto.getRandomValues(a);
  return [...a].map(b => b.toString(16).padStart(2, "0")).join("");
}

async function save() {
  err("");
  try {
    const r = await fetch("/api/settings", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(collect()) });
    const d = await r.json().catch(() => ({}));
    if (!r.ok || !d.ok) { err((d && d.error) || "could not save"); return; }
  } catch { err("network error"); return; }
  close();
  // reload so the rail/data reflect the new collector config
  location.reload();
}
