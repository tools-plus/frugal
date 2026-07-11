"use strict";
// Left-hand navigation: the service rail (column 1), the flat resource list
// and the EKS drill-down tree (column 2), plus the time-range pills and the
// EKS context-filter bar in the main header.

import {
  S, SVCMETA, NS2SVC, RANGES, RING_SPAN, BOOT_T,
  svcOf, isLive, svcConfigured, loadingOr, KIND_ORDER, KIND_PLURAL,
} from "./state.js";
import { renderMain } from "./charts.js";
import { closeLogStream } from "./logs.js";
import { iconSVG } from "./icons.js";

// ---------------- instant tooltip ----------------
// The native `title` attribute only appears after a ~1s browser delay, which
// feels laggy on the rail. This renders a fixed-position tooltip immediately on
// hover. Delegated on #rail (which persists across buildRail rebuilds) and
// positioned to the right of the hovered tile so #rail's overflow can't clip it.
let tipEl = null, tipsWired = false;
function railTip() {
  if (!tipEl) { tipEl = document.createElement("div"); tipEl.id = "tip"; document.body.appendChild(tipEl); }
  return tipEl;
}
function wireTips(rail) {
  if (tipsWired) return;
  tipsWired = true;
  rail.addEventListener("mousemove", e => {
    const tile = e.target.closest(".tile");
    if (!tile || !tile.dataset.tip) { railTip().classList.remove("show"); return; }
    const tip = railTip();
    tip.textContent = tile.dataset.tip;
    const r = tile.getBoundingClientRect();
    tip.style.top = (r.top + r.height / 2) + "px";
    tip.style.left = (r.right + 10) + "px";
    tip.classList.add("show");
  });
  rail.addEventListener("mouseleave", () => railTip().classList.remove("show"));
}

// ---------------- rail ----------------
export function buildRail() {
  const seen = new Map();
  for (const meta of S.series.values()) {
    const s = svcOf(meta);
    seen.set(s, (seen.get(s)||0) + 1);
  }
  const svcs = [...new Set([...Object.keys(SVCMETA), ...seen.keys()])].sort((a,b) => {
    const oa = SVCMETA[a] ? SVCMETA[a].ord : 99, ob = SVCMETA[b] ? SVCMETA[b].ord : 99;
    return oa - ob || a.localeCompare(b);
  });
  const rail = document.getElementById("rail");
  wireTips(rail);
  rail.innerHTML = "";
  let sect = null;
  for (const s of svcs) {
    const want = isLive(s) ? "live" : "aws";
    if (sect !== want) {
      sect = want;
      const d = document.createElement("div");
      d.className = "rail-sect"; d.textContent = sect;
      rail.appendChild(d);
    }
    const n = seen.get(s) || 0;
    const conf = svcConfigured(s);
    if (!n && conf === false) continue; // not collected: don't show a dead tile
    const b = document.createElement("button");
    b.className = "tile" + (s === S.service ? " active" : "") + (n ? "" : " nodata");
    let state = n ? n + " series" : "loading…";
    if (!n && conf === null && s === "Hosts") state = "no agents reporting yet";
    if (!n && Date.now() - BOOT_T > 120000) state = "configured, but no data yet";
    const tip = (SVCMETA[s] ? SVCMETA[s].title : s) + " · " + state;
    b.dataset.tip = tip;              // custom instant tooltip (see wireTips)
    b.setAttribute("aria-label", tip);
    const icon = iconSVG(s);
    if (icon) b.innerHTML = icon;                                   // glyph
    else b.textContent = SVCMETA[s] ? SVCMETA[s].abbr : s.slice(0, 4).toUpperCase(); // fallback
    b.onclick = () => selectService(s);
    rail.appendChild(b);
  }
  if (!S.service) {
    const withData = svcs.find(s => seen.get(s));
    if (withData) selectService(withData);
  }
}

export function selectService(svc) {
  S.service = svc; S.resource = null; S.sel = null; S.rsearch = "";
  // S3 storage metrics are emitted once per day by AWS, so the default 1h
  // window is always empty. Open S3 on a multi-day range (only then does the
  // client fetch /api/history, which returns the daily points).
  if (svc === "S3" && S.range < 86400) S.range = 604800;
  document.getElementById("ressearch").value = "";
  buildRail(); buildResList();
  if (svc === "EKS") {
    const cs = eksClusters();
    if (cs.length) { S.exp.add("c:" + cs[0]); select({t:"cp", cluster: cs[0], view:"metrics"}); }
    else renderMain();
  } else {
    const items = resourcesOf(svc);
    if (items.length) selectResource(items[0][0]); else renderMain();
  }
}

// ---------------- flat resource list (non-EKS) ----------------
export function resourcesOf(svc) {
  const map = new Map();
  for (const meta of S.series.values()) {
    if (svcOf(meta) !== svc) continue;
    const r = (meta.labels||{}).resource || "_aggregate";
    map.set(r, (map.get(r)||0) + 1);
  }
  return [...map.entries()].sort((a,b) => a[0].localeCompare(b[0]));
}
export function buildResList() {
  document.getElementById("svcname").textContent = SVCMETA[S.service] ? SVCMETA[S.service].title : (S.service||"—");
  if (S.service === "EKS") { buildEKSNav(); return; }
  const items = resourcesOf(S.service).filter(([r]) => !S.rsearch || r.toLowerCase().includes(S.rsearch));
  document.getElementById("svccnt").textContent = items.length;
  const box = document.getElementById("resitems");
  box.innerHTML = "";
  for (const [r, n] of items) {
    const b = document.createElement("button");
    b.className = "res" + (r === S.resource ? " active" : "");
    b.innerHTML = `<span class="nm">${r === "_aggregate" ? "(aggregate)" : r}</span><span class="meta">${n}</span>`;
    b.onclick = () => selectResource(r);
    box.appendChild(b);
  }
}
export function selectResource(r) { S.resource = r; S.sel = null; buildResList(); renderMain(); }

// ---------------- EKS nav (shallow) ----------------
export function eksClusters() {
  const set = new Set();
  for (const p of S.pods) if (p.cluster) set.add(p.cluster);
  for (const m of S.series.values()) {
    const L = m.labels || {};
    if (L.source === "k8s" && L.cluster) set.add(L.cluster);
    if (L.source === "cloudwatch" && L.namespace === "AWS/EKS" && L.resource) set.add(L.resource);
  }
  return [...set].sort();
}
export function clusterNodes(cluster) {
  const set = new Set();
  for (const m of S.series.values()) {
    const L = m.labels || {};
    if (L.source === "k8s" && L.kind === "node" && L.cluster === cluster) set.add(L.node);
  }
  return [...set].sort();
}
export function podsOf(cluster, kind, ns, wl) {
  return S.pods.filter(p => p.cluster === cluster
    && (!kind || (p.workloadKind || "Pod") === kind)
    && (!ns || p.namespace === ns)
    && (!wl || (p.workload || p.name) === wl));
}
function kindCounts(cluster) { // kind -> distinct workload count
  const m = new Map();
  for (const p of S.pods) {
    if (p.cluster !== cluster) continue;
    const k = p.workloadKind || "Pod";
    if (!m.has(k)) m.set(k, new Set());
    m.get(k).add(p.namespace + "/" + (p.workload || p.name));
  }
  return m;
}
const selKey = s => s ? [s.t, s.cluster, s.kind, s.ns, s.wl, s.pod, s.node].join("~") : "";
export function select(sel) { S.sel = sel; S.resource = null; closeLogStream(); buildEKSNav(); renderMain(); }
export function toggle(key) { S.exp.has(key) ? S.exp.delete(key) : S.exp.add(key); buildEKSNav(); }

function treeRow(box, depth, opts) {
  const b = document.createElement("button");
  b.className = "res" + (opts.active ? " active" : "");
  b.style.paddingLeft = (10 + depth * 14) + "px";
  const caret = `<span class="caret">${opts.caret === undefined ? "" : (opts.caret ? "▾" : "▸")}</span>`;
  b.innerHTML = `${caret}${opts.dot || ""}<span class="nm">${opts.label}</span>` +
    (opts.tag !== undefined ? `<span class="meta">${opts.tag}</span>` : "");
  b.onclick = opts.onclick;
  box.appendChild(b);
}

export function buildEKSNav() {
  const box = document.getElementById("resitems");
  box.innerHTML = "";
  const clusters = eksClusters();
  document.getElementById("svccnt").textContent = clusters.length + " clusters";
  const k = selKey(S.sel);

  if (S.rsearch) { // flat pod search across clusters
    let shown = 0;
    for (const p of S.pods) {
      const hay = (p.cluster + "/" + p.namespace + "/" + p.name + "/" + (p.workload||"")).toLowerCase();
      if (!hay.includes(S.rsearch)) continue;
      if (++shown > 200) break;
      treeRow(box, 0, {
        label: p.namespace + "/" + p.name, tag: p.cluster,
        dot: `<span class="pdot ${p.phase}"></span>`,
        onclick: () => select({t:"kind", cluster:p.cluster, kind:p.workloadKind || "Pod",
                               ns:p.namespace, wl:p.workload || p.name, pod:p.name, view:"metrics"}),
      });
    }
    if (!shown) box.innerHTML = `<div class="empty" style="padding:30px 10px">no matches</div>`;
    return;
  }

  for (const c of clusters) {
    const open = S.exp.has("c:" + c);
    treeRow(box, 0, {
      label: c, caret: open,
      active: k === selKey({t:"cp", cluster:c}),
      onclick: () => {
        const wasOpen = S.exp.has("c:" + c);
        const wasSel = k === selKey({t:"cp", cluster:c});
        for (const e of [...S.exp]) if (e.startsWith("c:")) S.exp.delete(e);
        if (!(wasOpen && wasSel)) S.exp.add("c:" + c);
        select({t:"cp", cluster:c, view:"metrics"});
      },
    });
    if (!open) continue;
    treeRow(box, 1, {
      label: "Control plane",
      active: k === selKey({t:"cp", cluster:c}),
      onclick: () => select({t:"cp", cluster:c, view:"metrics"}),
    });
    const nodes = clusterNodes(c);
    treeRow(box, 1, {
      label: "Nodes", tag: nodes.length,
      active: S.sel && S.sel.t === "nodes" && S.sel.cluster === c,
      onclick: () => select({t:"nodes", cluster:c, node:"", view:"metrics"}),
    });
    const kinds = kindCounts(c);
    for (const kind of KIND_ORDER) {
      if (!kinds.has(kind)) continue;
      treeRow(box, 1, {
        label: KIND_PLURAL[kind] || kind, tag: kinds.get(kind).size,
        active: S.sel && S.sel.t === "kind" && S.sel.cluster === c && S.sel.kind === kind,
        onclick: () => {
          const pods = podsOf(c, kind);
          const nss = [...new Set(pods.map(p => p.namespace))].sort();
          const ns = nss[0] || "";
          const wls = [...new Set(podsOf(c, kind, ns).map(p => p.workload || p.name))].sort();
          select({t:"kind", cluster:c, kind, ns, wl: wls[0] || "", pod:"", view:"metrics"});
        },
      });
    }
  }
  if (!clusters.length) box.innerHTML = `<div class="empty" style="padding:30px 10px">${loadingOr("no clusters yet - check kubernetes.contexts config")}</div>`;
}

// ---------------- time pills ----------------
export function buildPills() {
  const box = document.getElementById("pills");
  box.innerHTML = "";
  const sel = S.sel;
  const inLogs = sel && sel.view === "logs";
  box.style.display = inLogs ? "none" : "";
  document.getElementById("metricfilter").style.display = inLogs ? "none" : "";
  const longOK = S.service !== "Hosts" && !(S.service === "EKS" && sel && sel.t !== "cp");
  for (const [secs, label] of RANGES) {
    const b = document.createElement("button");
    b.className = "pill" + (secs === S.range ? " active" : "") + (!longOK && secs > RING_SPAN ? " hidden" : "");
    b.textContent = label;
    b.onclick = () => { S.range = secs; renderMain(); };
    box.appendChild(b);
  }
}

// ---------------- context filter bar ----------------
export function mkSelect(opts, value, onchange) {
  const sel = document.createElement("select");
  for (const [v, label] of opts) {
    const o = document.createElement("option");
    o.value = v; o.textContent = label;
    if (v === value) o.selected = true;
    sel.appendChild(o);
  }
  sel.onchange = () => onchange(sel.value);
  return sel;
}
function mkToggle(view) {
  const box = document.createElement("div");
  box.className = "pills";
  for (const v of ["metrics", "logs"]) {
    const b = document.createElement("button");
    b.className = "pill" + (view === v ? " active" : "");
    b.textContent = v;
    b.onclick = () => {
      const sel = {...S.sel, view: v};
      if (v === "logs" && !sel.pod) { // logs need one pod
        const pods = podsOf(sel.cluster, sel.kind, sel.ns, sel.wl);
        sel.pod = pods.length ? pods[0].name : "";
      }
      select(sel);
    };
    box.appendChild(b);
  }
  return box;
}
export function buildCtxFilters() {
  let bar = document.getElementById("ctxfilters");
  if (!bar) {
    bar = document.createElement("div");
    bar.id = "ctxfilters";
    const rn = document.getElementById("rname");
    rn.parentNode.insertBefore(bar, rn.nextSibling);
  }
  bar.innerHTML = "";
  const sel = S.sel;
  if (!sel || S.service !== "EKS") return;

  if (sel.t === "nodes") {
    const nodes = clusterNodes(sel.cluster);
    bar.append(mkSelect([["", "All nodes"], ...nodes.map(n => [n, n])], sel.node,
      v => select({...sel, node: v})));
    return;
  }
  if (sel.t === "kind") {
    const nss = [...new Set(podsOf(sel.cluster, sel.kind).map(p => p.namespace))].sort();
    bar.append(mkSelect(nss.map(n => [n, n]), sel.ns, v => {
      const wls = [...new Set(podsOf(sel.cluster, sel.kind, v).map(p => p.workload || p.name))].sort();
      select({...sel, ns: v, wl: wls[0] || "", pod: "", view: "metrics"});
    }));
    const wls = [...new Set(podsOf(sel.cluster, sel.kind, sel.ns).map(p => p.workload || p.name))].sort();
    bar.append(mkSelect(wls.map(w => [w, w]), sel.wl, v => select({...sel, wl: v, pod: "", view: "metrics"})));
    const pods = podsOf(sel.cluster, sel.kind, sel.ns, sel.wl).sort((a,b) => a.name.localeCompare(b.name));
    bar.append(mkSelect([["", "All pods"], ...pods.map(p => [p.name, p.name])], sel.pod,
      v => select({...sel, pod: v, view: v ? sel.view : "metrics"})));
    bar.append(mkToggle(sel.view));
  }
}
