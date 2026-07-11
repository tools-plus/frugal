"use strict";
// Entry point (loaded as <script type="module">). Wires up the search/filter
// inputs, boots from /api/series + /api/status, and polls /api/pods.

import { S } from "./state.js";
import { buildRail, buildResList, buildEKSNav, restoreNav } from "./nav.js";
import { renderMain } from "./charts.js";
import { connectStream } from "./stream.js";
import { openUsers } from "./users.js";

// ---------------- inputs ----------------
document.getElementById("ressearch").addEventListener("input", e => { S.rsearch = e.target.value.toLowerCase(); buildResList(); });
document.getElementById("metricfilter").addEventListener("input", e => { S.mfilter = e.target.value.toLowerCase(); renderMain(); });

// ---------------- auth (header user menu) ----------------
async function initAuth() {
  let me = {};
  try { me = await fetch("/api/me").then(r => r.json()); } catch { return; }
  if (!me.enabled || !me.authenticated) return;   // auth disabled (or, unexpectedly, not signed in)
  document.getElementById("username").textContent = me.user || "";
  document.getElementById("usermenu").hidden = false;
  document.getElementById("logout").onclick = async () => {
    try { await fetch("/api/logout", { method: "POST" }); } catch {}
    location.href = "/login";
  };
  document.getElementById("changepw").onclick = () => { location.href = "/login?change=1"; };
  if (me.role === "admin") {                       // user management is admin-only
    const ub = document.getElementById("usersbtn");
    ub.hidden = false;
    ub.onclick = () => openUsers(me.user);
  }
}

// ---------------- boot ----------------
async function refreshPods() {
  try {
    const pods = await fetch("/api/pods").then(r => r.ok ? r.json() : null);
    if (pods) {
      const had = S.pods.length;
      S.pods = pods;
      if (S.service === "EKS" && !S.rsearch) buildEKSNav();
      if (!had && pods.length && S.service === "EKS") renderMain();
      return pods.length;
    }
  } catch {}
  return S.pods.length;
}
async function boot() {
  // Paint immediately from the series list; pods fill in asynchronously.
  try {
    const series = await fetch("/api/series").then(r => r.json());
    for (const m of series) S.series.set(m.id, m);
  } catch (e) { console.error(e); }
  initAuth();
  if (!restoreNav()) buildRail();   // restore last view, else auto-select first
  connectStream();
  (async () => { // pods: quick retries while collectors warm up, then steady 30s
    for (let i = 0; i < 4; i++) {
      if (await refreshPods() > 0) break;
      await new Promise(r => setTimeout(r, 4000));
    }
    setInterval(refreshPods, 30000);
  })();
  fetch("/api/status").then(r => r.json()).then(st => {
    S.status = st;
    buildRail();
    const aws = st && st.aws;
    if (aws && aws.last_error) {
      document.getElementById("dot").title = "aws collector: " + aws.last_error;
      console.warn("aws collector:", aws.last_error);
    }
  }).catch(() => {});
}
boot();
