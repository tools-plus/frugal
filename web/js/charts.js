"use strict";
// Main content area: turn the selected service/resource/EKS node into groups
// of metrics, one uPlot chart per metric. `uPlot` is a global provided by the
// vendored /vendor/uPlot.iife.min.js loaded before this module.

import { S, MEMBER_CAP, PALETTE, svcOf, loadingOr, saveNav } from "./state.js";
import { metricFmt } from "./format.js";
import { ensureRing, ensureHistory, lastVal, windowed } from "./data.js";
import { buildPills, buildCtxFilters } from "./nav.js";
import { renderLogsInline, openAgentLogs } from "./logs.js";
import { openZoom } from "./zoom.js";

const axisStyle = { stroke: "#6b7a8f", grid: {stroke: "#1c2531", width: 1}, ticks: {stroke: "#1c2531"} };

function groupsByMetric(metas) {
  const byMetric = new Map();
  for (const m of metas) {
    const L = m.labels || {};
    if (S.mfilter && !(L.metric||"").toLowerCase().includes(S.mfilter)) continue;
    if (!byMetric.has(L.metric)) byMetric.set(L.metric, []);
    byMetric.get(L.metric).push(m);
  }
  return [...byMetric.keys()].sort().map(metric => {
    const ms = byMetric.get(metric).sort((a,b) => (a.labels.variant||"").localeCompare(b.labels.variant||"")).slice(0, MEMBER_CAP);
    return {metric, members: ms.map((m, i) => ({id: m.id, variant: m.labels.variant || m.labels.stat || "", color: PALETTE[i % PALETTE.length]}))};
  });
}
// overlay group: one chart per metric, one line per entity, top-N by usage
function overlayGroups(metas, nameOf, total) {
  const byMetric = new Map();
  for (const m of metas) {
    const metric = m.labels.metric;
    if (S.mfilter && !metric.toLowerCase().includes(S.mfilter)) continue;
    if (!byMetric.has(metric)) byMetric.set(metric, []);
    byMetric.get(metric).push(m);
  }
  return [...byMetric.keys()].sort().map(metric => {
    let ms = byMetric.get(metric);
    ms.sort((a,b) => (lastVal(b.id) ?? -1) - (lastVal(a.id) ?? -1));
    const over = Math.max(0, ms.length - MEMBER_CAP);
    ms = ms.slice(0, MEMBER_CAP);
    return {metric, over, members: ms.map((m, i) => ({id: m.id, variant: nameOf(m), color: PALETTE[i % PALETTE.length]}))};
  });
}

export function renderMain() {
  saveNav();   // persist the current selection so a refresh can restore it
  buildPills(); buildCtxFilters();
  for (const c of S.charts) c.u && c.u.destroy();
  S.charts = []; S.dirty.clear();
  const box = document.getElementById("charts");
  box.innerHTML = "";

  let title = "select a resource", groups = [], phase = "", hint = "";
  const metas = [...S.series.values()];
  const sel = S.sel;

  if (S.service === "EKS" && sel) {
    if (sel.t === "cp") {
      title = sel.cluster + " · control plane";
      groups = groupsByMetric(metas.filter(m => {
        const L = m.labels||{};
        return L.source === "cloudwatch" && L.namespace === "AWS/EKS" && L.resource === sel.cluster;
      }));
      const cwEKS = [...new Set(metas.filter(m => (m.labels||{}).source === "cloudwatch" && m.labels.namespace === "AWS/EKS").map(m => m.labels.resource))].sort();
      hint = 'no AWS/EKS CloudWatch metrics for "' + sel.cluster + '"' + String.fromCharCode(10) +
        (cwEKS.length ? "CloudWatch reports clusters: " + cwEKS.join(", ")
                      : "no AWS/EKS series at all - check the AWS collector region/profile in the server log");
    } else if (sel.t === "nodes") {
      const nm = metas.filter(m => {
        const L = m.labels||{};
        return L.source === "k8s" && L.kind === "node" && L.cluster === sel.cluster
          && (!sel.node || L.node === sel.node);
      });
      title = sel.cluster;
      groups = sel.node ? groupsByMetric(nm) : overlayGroups(nm, m => m.labels.node);
    } else if (sel.t === "kind") {
      const pm = metas.filter(m => {
        const L = m.labels||{};
        return L.source === "k8s" && L.kind === "pod" && L.cluster === sel.cluster
          && L.namespace === sel.ns && (L.workload || L.pod) === sel.wl
          && (!sel.pod || L.pod === sel.pod);
      });
      title = sel.cluster;
      if (sel.pod) {
        const info = S.pods.find(p => p.cluster === sel.cluster && p.namespace === sel.ns && p.name === sel.pod);
        phase = info ? info.phase : "";
        if (sel.view === "logs") {
          document.getElementById("rname").textContent = title;
          setPhase(phase);
          renderLogsInline(box, info || {cluster: sel.cluster, namespace: sel.ns, name: sel.pod, containers: []});
          return;
        }
        groups = groupsByMetric(pm);
      } else {
        groups = overlayGroups(pm, m => m.labels.pod);
      }
      hint = "no pod metrics yet — is metrics-server running in this cluster?";
    }
  } else if (S.resource) {
    title = S.resource === "_aggregate" ? "(aggregate)" : S.resource;
    groups = groupsByMetric(metas.filter(m =>
      svcOf(m) === S.service && ((m.labels||{}).resource || "_aggregate") === S.resource));
    if (S.service === "Hosts" && S.charts) { /* logs via button below */ }
  }

  document.getElementById("rname").textContent = title;
  setPhase(phase);
  const lb = document.getElementById("logsbtn");
  const hostLogs = S.service === "Hosts" && S.resource;
  lb.style.display = hostLogs ? "" : "none";
  lb.onclick = hostLogs ? () => openAgentLogs(S.resource) : null;

  if (!groups.length) {
    box.innerHTML = `<div class="empty">${hint || loadingOr("no metrics here yet")}</div>`;
    return;
  }
  const grid = document.createElement("div");
  grid.className = "grid";
  box.appendChild(grid);

  const gen = renderMain.gen = (renderMain.gen||0) + 1;
  for (const g of groups) {
    const fmt = metricFmt(g.metric);
    const card = document.createElement("div");
    card.className = "card";
    card.innerHTML = `<div class="hd"><span class="title">${g.metric}</span><span class="val">–</span>`
      + `<button class="maxbtn" title="Maximize" aria-label="Maximize">`
      + `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M8 3H3v5M16 3h5v5M8 21H3v-5M16 21h5v-5"/></svg>`
      + `</button></div><div class="plot"></div>`;
    if (g.members.length > 1) {
      const lg = document.createElement("div");
      lg.className = "legend";
      lg.innerHTML = g.members.map(m => `<span><span class="sw" style="background:${m.color}"></span>${m.variant || "value"}</span>`).join("")
        + (g.over ? `<span>+${g.over} more (top ${MEMBER_CAP} by current value)</span>` : "");
      card.appendChild(lg);
    }
    grid.appendChild(card);
    const chart = {u: null, metric: g.metric, members: g.members, fmt, card, valEl: card.querySelector(".val")};
    card.querySelector(".maxbtn").onclick = () => openZoom(chart);
    S.charts.push(chart);
    Promise.all(g.members.map(m => ensureRing(m.id).then(() => ensureHistory(m.id, S.range)))).then(() => {
      if (renderMain.gen !== gen) return;
      drawChart(chart);
    });
  }
}
function setPhase(phase) {
  const ph = document.getElementById("rphase");
  ph.textContent = phase; ph.className = "phase " + phase;
}

function drawChart(chart) {
  const el = chart.card.querySelector(".plot");
  const data = windowed(chart.members.map(m => m.id));
  if (chart.u) { chart.u.setData(data); updateVal(chart, data); return; }
  if (!data[0].length) { el.innerHTML = `<div class="nodata">no data in range</div>`; chart.pending = true; updateVal(chart, data); return; }
  el.innerHTML = "";
  chart.pending = false;
  const opts = {
    width: el.clientWidth || 400, height: 150,
    cursor: {points: {show: false}},
    padding: [8, 8, 0, 0],
    scales: { x: { range: () => { const n = Date.now()/1000; return [n - S.range, n]; } } },
    series: [ {}, ...chart.members.map(m => ({label: m.variant, stroke: m.color, width: 1.5, spanGaps: true,
                fill: chart.members.length === 1 ? m.color + "14" : undefined})) ],
    axes: [
      {...axisStyle, values: (u, vals) => vals.map(t => {
        const d = new Date(t*1000);
        return S.range > 86400 ? (d.getMonth()+1)+"/"+d.getDate()+" "+String(d.getHours()).padStart(2,"0")+"h"
                               : d.toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"});
      })},
      {...axisStyle, size: 64, values: (u, vals) => vals.map(chart.fmt)},
    ],
  };
  chart.u = new uPlot(opts, data, el);
  new ResizeObserver(() => chart.u && chart.u.setSize({width: el.clientWidth, height: 150})).observe(el);
  updateVal(chart, data);
}
function updateVal(chart, data) {
  let last = null;
  const col = data[1] || [];
  for (let i = col.length - 1; i >= 0; i--) if (col[i] != null) { last = col[i]; break; }
  chart.valEl.textContent = chart.fmt(last);
}
setInterval(() => {
  for (const idx of S.dirty) {
    const c = S.charts[idx];
    if (c && (c.u || c.pending)) drawChart(c);
  }
  S.dirty.clear();
}, 1000);
