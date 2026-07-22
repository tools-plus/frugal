"use strict";
// Live SSE stream: every new point fans in here, lands in S.data, and marks
// affected charts dirty (redrawn by the interval in charts.js). Also drives
// the header connection dot + a liveness readout.
//
// The readout is "<N> series · updated <age>", where age is how old the newest
// datapoint is. A raw "points per interval" number is misleading here: with
// CloudWatch polled every ~5 min, most short windows are legitimately 0 even
// when everything is healthy. The age is seeded from the series already loaded
// (so a fresh page load shows a real age immediately, not "no data yet") and
// then kept current from the live stream.

import { S, svcOf } from "./state.js";
import { buildRail, buildResList } from "./nav.js";

let newestT = 0;    // unix seconds of the newest datapoint seen
let seeded = false; // whether newestT has been seeded from the loaded series list

export function connectStream() {
  const es = new EventSource("/api/stream");
  const dot = document.getElementById("dot");
  es.onopen = () => dot.classList.add("on");
  es.onerror = () => dot.classList.remove("on");
  es.addEventListener("point", ev => {
    const u = JSON.parse(ev.data);
    if (u.point && u.point.t > newestT) newestT = u.point.t;
    let isNew = false;
    if (!S.series.has(u.id)) { S.series.set(u.id, {id: u.id, labels: u.labels}); isNew = true; }
    const m = S.data.get(u.id) || new Map();
    m.set(u.point.t, u.point.v);
    S.data.set(u.id, m);
    if (isNew) { buildRail(); if (svcOf({labels: u.labels || {}}) === S.service) buildResList(); return; }
    S.charts.forEach((c, i) => { if (c.members.some(mb => mb.id === u.id)) S.dirty.add(i); });
  });
}

function ago(sec) {
  if (sec < 60) return sec + "s ago";
  if (sec < 3600) return Math.floor(sec / 60) + "m ago";
  return Math.floor(sec / 3600) + "h ago";
}

setInterval(() => {
  // Seed the newest timestamp from the initially-loaded series once available.
  if (!seeded && S.series.size) {
    for (const m of S.series.values()) if (m.last && m.last.t > newestT) newestT = m.last.t;
    seeded = true;
  }
  const nowSec = Math.floor(Date.now() / 1000);
  const readout = newestT ? "updated " + ago(Math.max(0, nowSec - newestT)) : "no data yet";
  document.getElementById("rate").textContent = S.series.size + " series · " + readout;
}, 1000);
