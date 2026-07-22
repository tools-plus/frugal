"use strict";
// Maximize view: click a chart card's maximize button to open a large modal of
// that single chart. Same time ranges as the dashboard (12h…7d) plus
// drag-to-zoom: select a segment to zoom in, double-click to reset. Zooming
// re-fetches that slice at finer resolution. The modal redraws on a 1s interval
// while open so it stays live.

import { S } from "./state.js";
import { windowed, ensureRing, ensureHistory, fetchWindow } from "./data.js";

const ZOOM_RANGES = [[43200, "12h"], [86400, "24h"], [259200, "3d"], [604800, "7d"]];
const axisStyle = { stroke: "#6b7a8f", grid: {stroke: "#1c2531", width: 1}, ticks: {stroke: "#1c2531"} };

let z = null;      // { chart, range, zoom, u, plotEl, valEl, timer }
let root = null;   // the #zoom modal element (built once)

function build() {
  if (root) return root;
  root = document.createElement("div");
  root.id = "zoom";
  root.innerHTML =
    '<div class="box">' +
      '<div class="zbar">' +
        '<span class="ztitle"></span>' +
        '<div class="pills zranges"></div>' +
        '<span class="zval"></span>' +
        '<button class="zclose" aria-label="Close">✕</button>' +
      '</div>' +
      '<div class="zlegend"></div>' +
      '<div class="zplot"></div>' +
    '</div>';
  document.body.appendChild(root);
  root.querySelector(".zclose").onclick = closeZoom;
  root.addEventListener("mousedown", e => { if (e.target === root) closeZoom(); }); // backdrop
  root.querySelector(".zplot").addEventListener("dblclick", resetZoom);            // reset zoom
  return root;
}

export function openZoom(chart) {
  build();
  z = {
    chart, range: 43200, zoom: null,
    plotEl: root.querySelector(".zplot"),
    valEl: root.querySelector(".zval"),
    u: null,
  };
  root.querySelector(".ztitle").textContent = chart.metric;

  // range pills
  const pills = root.querySelector(".zranges");
  pills.innerHTML = "";
  for (const [secs, label] of ZOOM_RANGES) {
    const b = document.createElement("button");
    b.className = "pill" + (secs === z.range ? " active" : "");
    b.textContent = label;
    b.onclick = () => setRange(secs);
    pills.appendChild(b);
  }

  // legend (only when more than one line)
  const lg = root.querySelector(".zlegend");
  lg.innerHTML = chart.members.length > 1
    ? chart.members.map(m => `<span><span class="sw" style="background:${m.color}"></span>${m.variant || "value"}</span>`).join("")
    : "";

  root.classList.add("open");
  document.addEventListener("keydown", onKey);
  loadAndDraw();
  z.timer = setInterval(draw, 1000);            // keep the maximized chart live
  new ResizeObserver(() => { if (z && z.u) z.u.setSize({ width: z.plotEl.clientWidth, height: z.plotEl.clientHeight }); }).observe(z.plotEl);
}

function setRange(secs) {
  if (!z) return;
  z.range = secs;
  z.zoom = null;
  for (const b of root.querySelectorAll(".zranges .pill")) b.classList.toggle("active", b.textContent === labelFor(secs));
  if (z.u) { z.u.destroy(); z.u = null; }        // rebuild so the x-scale rewindows
  z.plotEl.innerHTML = "";
  loadAndDraw();
}
const labelFor = secs => (ZOOM_RANGES.find(r => r[0] === secs) || [, ""])[1];

// loadAndDraw ensures the ring + long-range history for the chart's members are
// loaded for the current range, then draws. Draws immediately too so the modal
// isn't blank while the history fetch is in flight.
function loadAndDraw() {
  if (!z) return;
  const ids = z.chart.members.map(m => m.id), range = z.range;
  Promise.all(ids.map(id => ensureRing(id).then(() => ensureHistory(id, range)))).then(() => {
    if (z && z.range === range) draw();
  });
  draw();
}

function draw() {
  if (!z) return;
  const data = windowed(z.chart.members.map(m => m.id), z.range);
  // last non-null value for the readout
  let last = null; const col = data[1] || [];
  for (let i = col.length - 1; i >= 0; i--) if (col[i] != null) { last = col[i]; break; }
  z.valEl.textContent = z.chart.fmt(last);

  if (z.u) { z.u.setData(data); return; }
  if (!data[0].length) { z.plotEl.innerHTML = '<div class="nodata">no data in range</div>'; return; }
  z.plotEl.innerHTML = "";
  const el = z.plotEl, fmt = z.chart.fmt;
  const opts = {
    width: el.clientWidth || 800, height: el.clientHeight || 400,
    cursor: { points: { show: false }, drag: { x: true, y: false, setScale: false } },
    padding: [10, 12, 0, 0],
    scales: { x: { range: () => xRange() } },
    series: [ {}, ...z.chart.members.map(m => ({ label: m.variant, stroke: m.color, width: 1.5, spanGaps: true,
                fill: z.chart.members.length === 1 ? m.color + "14" : undefined })) ],
    axes: [
      {...axisStyle, values: (u, vals) => tickFmt(vals)},
      {...axisStyle, size: 64, values: (u, vals) => vals.map(fmt)},
    ],
    hooks: { setSelect: [u => dragZoom(u)] },
  };
  z.u = new uPlot(opts, data, el);
}

// xRange / tickFmt / dragZoom / resetZoom mirror the main-grid zoom behavior.
function xRange() {
  if (z.zoom) return [z.zoom.min, z.zoom.max];
  const n = Date.now() / 1000;
  return [n - z.range, n];
}
function tickFmt(vals) {
  const span = z.zoom ? (z.zoom.max - z.zoom.min) : z.range;
  return vals.map(t => {
    const d = new Date(t * 1000);
    return span > 86400 ? (d.getMonth()+1)+"/"+d.getDate()+" "+String(d.getHours()).padStart(2,"0")+"h"
                        : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  });
}
function dragZoom(u) {
  if (!z) return;
  // Read before clearing — u.setSelect mutates u.select in place.
  const left = u.select.left, width = u.select.width;
  u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
  if (width <= 8) return;
  const min = u.posToVal(left, "x");
  const max = u.posToVal(left + width, "x");
  if (max - min < 1) return;
  z.zoom = { min, max };
  u.setScale("x", { min, max });
  Promise.all(z.chart.members.map(m => fetchWindow(m.id, min, max))).then(() => { if (z && z.u) draw(); });
}
function resetZoom() {
  if (!z || !z.zoom || !z.u) return;
  z.zoom = null;
  const n = Date.now() / 1000;
  z.u.setScale("x", { min: n - z.range, max: n });
}

function onKey(e) { if (e.key === "Escape") closeZoom(); }

function closeZoom() {
  if (!z) return;
  clearInterval(z.timer);
  if (z.u) z.u.destroy();
  root.classList.remove("open");
  document.removeEventListener("keydown", onKey);
  z = null;
}
