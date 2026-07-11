"use strict";
// Live SSE stream: every new point fans in here, lands in S.data, and marks
// affected charts dirty (redrawn by the interval in charts.js). Also drives
// the header connection dot + points/sec readout.

import { S, svcOf } from "./state.js";
import { buildRail, buildResList } from "./nav.js";

export function connectStream() {
  const es = new EventSource("/api/stream");
  const dot = document.getElementById("dot");
  es.onopen = () => dot.classList.add("on");
  es.onerror = () => dot.classList.remove("on");
  es.addEventListener("point", ev => {
    S.events++;
    const u = JSON.parse(ev.data);
    let isNew = false;
    if (!S.series.has(u.id)) { S.series.set(u.id, {id: u.id, labels: u.labels}); isNew = true; }
    const m = S.data.get(u.id) || new Map();
    m.set(u.point.t, u.point.v);
    S.data.set(u.id, m);
    if (isNew) { buildRail(); if (svcOf({labels: u.labels || {}}) === S.service) buildResList(); return; }
    S.charts.forEach((c, i) => { if (c.members.some(mb => mb.id === u.id)) S.dirty.add(i); });
  });
}
setInterval(() => {
  document.getElementById("rate").textContent = S.events + " pts/10s · " + S.series.size + " series";
  S.events = 0;
}, 10000);
