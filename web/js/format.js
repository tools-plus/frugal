"use strict";
// Value formatters. Pure — no shared state.

export const fmtBytes = v => {
  if (v == null || isNaN(v)) return "–";
  const u = ["B","KiB","MiB","GiB","TiB"]; let i = 0;
  while (Math.abs(v) >= 1024 && i < u.length-1) { v /= 1024; i++; }
  return v.toFixed(Math.abs(v) >= 100 ? 0 : 1) + " " + u[i];
};
export const fmtCores = v => v == null || isNaN(v) ? "–" : (v < 1 ? Math.round(v*1000) + "m" : v.toFixed(2));
export const fmtSecs = v => v == null || isNaN(v) ? "–" : (v < 0.001 ? (v*1e6).toFixed(0)+"µs" : v < 1 ? (v*1000).toFixed(1)+"ms" : v.toFixed(2)+"s");
export const fmtPct = v => v == null || isNaN(v) ? "–" : v.toFixed(1)+"%";
export const fmtNum = v => {
  if (v == null || isNaN(v)) return "–";
  if (Math.abs(v) >= 1e9) return (v/1e9).toFixed(1)+"G";
  if (Math.abs(v) >= 1e6) return (v/1e6).toFixed(1)+"M";
  if (Math.abs(v) >= 1e3) return (v/1e3).toFixed(1)+"k";
  return Math.abs(v) < 10 && v !== Math.round(v) ? v.toFixed(2) : String(Math.round(v));
};
export function metricFmt(name) {
  if (name === "cpu_cores") return fmtCores;
  if (/memory_bytes|Memory|Bytes|Storage|MemUsed|MemLimit|DiskFree/i.test(name)) return fmtBytes;
  if (/Latency|ResponseTime|duration_seconds/i.test(name)) return fmtSecs;
  if (/Utilization|Percentage|Pressure|Ratio|_pct$/i.test(name)) return fmtPct;
  return fmtNum;
}
