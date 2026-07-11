"use strict";
// Log streaming over SSE — inline (EKS pod view in the main area) and the
// bottom drawer (Hosts agent logs). One EventSource at a time.

import { mkSelect } from "./nav.js";

let logES = null;
export function closeLogStream() { if (logES) { logES.close(); logES = null; } }
function pushLine(target, txt) {
  const atBottom = target.scrollTop + target.clientHeight >= target.scrollHeight - 40;
  const sp = txt.indexOf(" ");
  const div = document.createElement("div");
  if (sp > 0) div.innerHTML = `<span class="ts">${txt.slice(0, sp)}</span> ${escapeHTML(txt.slice(sp+1))}`;
  else div.textContent = txt;
  target.appendChild(div);
  while (target.children.length > 2000) target.removeChild(target.firstChild);
  const follow = document.getElementById("follow");
  if ((follow && target.id === "loglines") ? follow.checked : atBottom) target.scrollTop = target.scrollHeight;
}
function startLogURL(url, target) {
  closeLogStream();
  target.textContent = "";
  logES = new EventSource(url);
  logES.addEventListener("log", ev => pushLine(target, JSON.parse(ev.data)));
  logES.addEventListener("error", ev => { if (ev.data) pushLine(target, "!! " + JSON.parse(ev.data)); });
  logES.addEventListener("eof", () => pushLine(target, "-- log stream ended --"));
}
function logsURLFor(pod, container) {
  return `/api/logs?cluster=${encodeURIComponent(pod.cluster||"")}&namespace=${encodeURIComponent(pod.namespace)}&pod=${encodeURIComponent(pod.name)}&container=${encodeURIComponent(container||"")}&tail=200`;
}
// inline (main area) log view for EKS pod selections
export function renderLogsInline(box, pod) {
  const wrap = document.createElement("div");
  wrap.id = "mainlogwrap";
  const bar = document.createElement("div");
  bar.style.cssText = "display:flex;align-items:center;gap:10px;margin-bottom:10px;";
  if ((pod.containers || []).length > 1) {
    const lbl = document.createElement("span");
    lbl.className = "lbl"; lbl.textContent = "container";
    lbl.style.cssText = "font-size:10px;letter-spacing:.08em;text-transform:uppercase;color:var(--muted)";
    const csel = mkSelect(pod.containers.map(c => [c, c]), pod.containers[0],
      v => startLogURL(logsURLFor(pod, v), document.getElementById("mainlog")));
    bar.append(lbl, csel);
  }
  const pre = document.createElement("pre");
  pre.id = "mainlog";
  wrap.append(bar, pre);
  box.appendChild(wrap);
  startLogURL(logsURLFor(pod, (pod.containers || [])[0] || ""), pre);
}
// drawer (Hosts)
export function openAgentLogs(host) {
  document.getElementById("logname").textContent = host;
  document.getElementById("logcontainer").style.display = "none";
  document.getElementById("drawer").classList.add("open");
  startLogURL(`/api/agentlogs?source=${encodeURIComponent("host/" + host)}&tail=200`, document.getElementById("loglines"));
}
function escapeHTML(s) { return s.replace(/[&<>]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[c])); }
document.getElementById("closelog").onclick = () => {
  document.getElementById("drawer").classList.remove("open");
  closeLogStream();
};
