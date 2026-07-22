"use strict";
// Data plumbing: fetch ring buffers / on-demand history into S.data, and
// derive last values and windowed matrices for charting.

import { S, RING_SPAN } from "./state.js";

export async function ensureRing(id) {
  if (S.ringLoaded.has(id)) return;
  S.ringLoaded.add(id);
  const pts = await fetch("/api/series/data?id=" + encodeURIComponent(id)).then(r => r.json()).catch(() => []);
  const m = S.data.get(id) || new Map();
  for (const p of pts) m.set(p.t, p.v);
  S.data.set(id, m);
}
export async function ensureHistory(id, range) {
  if (range <= RING_SPAN) return;
  // All sources now have long-range history: CloudWatch from the CloudWatch API,
  // everything else (k8s/agent/native) from the persisted SQLite store.
  if ((S.histRange.get(id) || 0) >= range) return;
  S.histRange.set(id, range);
  const now = Math.floor(Date.now()/1000);
  const pts = await fetch(`/api/history?id=${encodeURIComponent(id)}&from=${now-range}&to=${now}`)
    .then(r => r.ok ? r.json() : []).catch(() => []);
  const m = S.data.get(id) || new Map();
  for (const p of pts) m.set(p.t, p.v);
  S.data.set(id, m);
}
// fetchWindow re-fetches a specific [from,to] window at its natural (finer)
// resolution and merges it in — used when the user drag-zooms into a slice.
// CloudWatch series come back finer from the CloudWatch API; other sources come
// from SQLite at the span's downsample step. Cached per id so repeated zooms
// into an already-covered window don't refetch.
const fetchedWindows = new Map(); // id -> [[from,to], ...] already fetched
export async function fetchWindow(id, from, to) {
  from = Math.floor(from); to = Math.ceil(to);
  if (to <= from) return;
  const seen = fetchedWindows.get(id) || [];
  if (seen.some(([f, t]) => f <= from && t >= to)) return; // already covered
  seen.push([from, to]);
  fetchedWindows.set(id, seen);
  const pts = await fetch(`/api/history?id=${encodeURIComponent(id)}&from=${from}&to=${to}`)
    .then(r => r.ok ? r.json() : []).catch(() => []);
  const m = S.data.get(id) || new Map();
  for (const p of pts) m.set(p.t, p.v);
  S.data.set(id, m);
}
export function lastVal(id) {
  const m = S.data.get(id);
  if (m && m.size) { let mt = -1, mv = null; for (const [t,v] of m) if (t > mt) { mt = t; mv = v; } return mv; }
  const meta = S.series.get(id);
  return meta && meta.last ? meta.last.v : null;
}
export function windowed(ids, range = S.range) {
  const now = Math.floor(Date.now()/1000), from = now - range;
  const xset = new Set();
  for (const id of ids) {
    const m = S.data.get(id);
    if (m) for (const t of m.keys()) if (t >= from) xset.add(t);
  }
  const xs = [...xset].sort((a,b) => a-b);
  const cols = ids.map(id => {
    const m = S.data.get(id) || new Map();
    return xs.map(t => m.has(t) ? m.get(t) : null);
  });
  return [xs, ...cols];
}
