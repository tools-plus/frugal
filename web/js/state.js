"use strict";
// Shared dashboard state, service metadata, and small helpers derived from it.
// Everything hangs off the single mutable `S` object; other modules import it
// and mutate it directly (same as the original single-file global).

export const S = {
  series: new Map(), data: new Map(), ringLoaded: new Set(), histRange: new Map(),
  charts: [], dirty: new Set(), pods: [],
  service: null,
  resource: null,   // flat services
  sel: null,        // EKS: {t:"cp"|"nodes"|"kind", cluster, kind, ns, wl, pod, node, view}
  exp: new Set(),
  range: 43200, mfilter: "", rsearch: "",
  status: null,
};

export function svcConfigured(svc) {
  if (!S.status) return null; // unknown yet
  if (svc === "EKS") return (S.status.clusters || []).length > 0 ||
    ((S.status.aws && S.status.aws.namespaces) || []).includes("AWS/EKS");
  if (svc === "Hosts") return null; // agents are push-based; can't know from config
  if ((S.status.native || []).includes(svc)) return true;
  const nsList = (S.status.aws && S.status.aws.namespaces) || [];
  for (const ns of nsList) {
    const short = ns.replace("AWS/", "");
    if ((NS2SVC[short] || short) === svc) return true;
  }
  return false;
}

export const RING_SPAN = 6 * 3600;
export const MEMBER_CAP = 12;
export const PALETTE = ["#3fbfb4","#e8a33d","#9a7fdd","#d4537e","#5b9bd5","#97c459","#f09595","#b4b2a9","#6fbf73","#e25d4e","#c9d4e3","#8a6d3b"];
export const RANGES = [[43200,"12h"],[86400,"24h"],[259200,"3d"],[604800,"7d"]];
export const BOOT_T = Date.now();
export const loadingOr = msg => (Date.now() - BOOT_T < 120000 ? "loading… collectors are warming up" : msg);
export const KIND_ORDER = ["Deployment","StatefulSet","DaemonSet","Job","CronJob","ReplicaSet","Rollout","Pod"];
export const KIND_PLURAL = {Deployment:"Deployments", StatefulSet:"StatefulSets", DaemonSet:"DaemonSets",
  Job:"Jobs", CronJob:"CronJobs", ReplicaSet:"ReplicaSets", Rollout:"Rollouts", Pod:"Pods"};

// ---------------- services ----------------
export const SVCMETA = {
  "EKS":         {abbr:"EKS",  title:"EKS clusters", live:true, ord:0},
  "Hosts":       {abbr:"HOST", title:"Host agents (EC2 / VMs)", live:true, ord:1},
  "EC2":         {abbr:"EC2",  title:"EC2 instances", ord:3},
  "ALB":         {abbr:"ALB",  title:"Application load balancers", ord:4},
  "NLB":         {abbr:"NLB",  title:"Network load balancers", ord:5},
  "RDS":         {abbr:"RDS",  title:"RDS databases", ord:6},
  "DocDB":       {abbr:"DOC",  title:"DocumentDB", ord:7},
  "Valkey":      {abbr:"VLK",  title:"Valkey / ElastiCache", ord:8},
  "MQ":          {abbr:"MQ",   title:"AmazonMQ brokers", ord:9},
  "OpenSearch":  {abbr:"OS",   title:"OpenSearch domains", ord:10},
  "S3":          {abbr:"S3",   title:"S3 buckets", ord:11},
  "Insights":    {abbr:"CI",   title:"Container Insights", ord:12},
};
export const NS2SVC = {EC2:"EC2", RDS:"RDS", DocDB:"DocDB", ElastiCache:"Valkey", AmazonMQ:"MQ",
  ES:"OpenSearch", S3:"S3", ApplicationELB:"ALB", NetworkELB:"NLB", EKS:"EKS", ContainerInsights:"Insights"};

export function svcOf(meta) {
  const L = meta.labels || {};
  if (L.source === "k8s") return "EKS";
  if (L.source === "agent") return "Hosts";
  if (L.source === "native") return L.svc || "Native";
  const ns = (L.namespace || "").replace("AWS/", "");
  return NS2SVC[ns] || ns || "Other";
}
export const isLive = svc => !!(SVCMETA[svc] && SVCMETA[svc].live);

// ---------------- nav persistence ----------------
// The current selection lives only in S, so a browser refresh would reset it.
// Persist the navigation state (which service/resource/EKS selection + range)
// to localStorage on every render and restore it on boot — see restoreNav().
const NAV_KEY = "frugal.nav";
export function saveNav() {
  try {
    localStorage.setItem(NAV_KEY, JSON.stringify({
      service: S.service, resource: S.resource, sel: S.sel, range: S.range,
    }));
  } catch { /* private mode / disabled storage: fall back to no persistence */ }
}
export function loadNav() {
  try { return JSON.parse(localStorage.getItem(NAV_KEY) || "null"); } catch { return null; }
}
