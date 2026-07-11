"use strict";
// Per-service rail icons: generic, relatable line glyphs (one visual language,
// not brand logos). They draw with `currentColor`, so the tile's CSS color
// drives them — muted when idle, bright on hover, accent when active — exactly
// like the text labels they replaced. Each entry is the inner SVG markup for a
// 0 0 24 24 viewBox; iconSVG() wraps it. buildRail() falls back to the text
// abbreviation for any service not listed here.

const ICONS = {
  // EKS → Kubernetes helm / wheel
  EKS: '<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="2.3"/>'
     + '<path d="M12 4v3.4M12 16.6V20M4 12h3.4M16.6 12H20M6.3 6.3l2.4 2.4M15.3 15.3l2.4 2.4M17.7 6.3l-2.4 2.4M8.7 15.3l-2.4 2.4"/>',
  // Hosts → stacked servers
  Hosts: '<rect x="3.5" y="4.5" width="17" height="6" rx="1.3"/><rect x="3.5" y="13.5" width="17" height="6" rx="1.3"/>'
       + '<circle cx="6.8" cy="7.5" r=".55" fill="currentColor" stroke="none"/><circle cx="6.8" cy="16.5" r=".55" fill="currentColor" stroke="none"/>'
       + '<path d="M9.5 7.5h7M9.5 16.5h7"/>',
  // EC2 → CPU / compute chip
  EC2: '<rect x="7" y="7" width="10" height="10" rx="1.2"/><rect x="10" y="10" width="4" height="4" rx=".6"/>'
     + '<path d="M9 4v3M12 4v3M15 4v3M9 17v3M12 17v3M15 17v3M4 9h3M4 12h3M4 15h3M17 9h3M17 12h3M17 15h3"/>',
  // ALB → application load balancer: one node fanning out to three (outline)
  ALB: '<path d="M7.1 11.2 16.9 6.3M7.2 12H17M7.1 12.8 16.9 17.7"/>'
     + '<circle cx="5" cy="12" r="2.2"/><circle cx="19" cy="5.5" r="2"/><circle cx="19" cy="12" r="2"/><circle cx="19" cy="18.5" r="2"/>',
  // NLB → network load balancer: same fan-out, filled nodes (distinguishes it from ALB)
  NLB: '<path d="M7.1 11.2 16.9 6.3M7.2 12H17M7.1 12.8 16.9 17.7"/>'
     + '<circle cx="5" cy="12" r="2.2" fill="currentColor" stroke="none"/><circle cx="19" cy="5.5" r="2" fill="currentColor" stroke="none"/>'
     + '<circle cx="19" cy="12" r="2" fill="currentColor" stroke="none"/><circle cx="19" cy="18.5" r="2" fill="currentColor" stroke="none"/>',
  // RDS → relational database cylinder
  RDS: '<ellipse cx="12" cy="6" rx="7" ry="2.8"/><path d="M5 6v12c0 1.55 3.13 2.8 7 2.8s7-1.25 7-2.8V6"/>'
     + '<path d="M5 12c0 1.55 3.13 2.8 7 2.8s7-1.25 7-2.8"/>',
  // DocDB → document (DocumentDB)
  DocDB: '<path d="M7 3.5h6.5L18 8v12.5H7z"/><path d="M13.5 3.5V8H18"/><path d="M9.5 12h6M9.5 15h6M9.5 18h4"/>',
  // Valkey → key (key-value cache)
  Valkey: '<circle cx="8" cy="9" r="3.5"/><path d="M10.5 11.5 19 20M16 17l2-2M13.4 14.4l2-2"/>',
  // MQ → message queue (envelope)
  MQ: '<rect x="3.5" y="6" width="17" height="12" rx="1.5"/><path d="M4.2 7l7.8 6 7.8-6"/>',
  // OpenSearch → magnifier
  OpenSearch: '<circle cx="10.5" cy="10.5" r="6"/><path d="M15 15l5 5"/>',
  // S3 → open-top storage pail: an elliptical open rim (not a flat lid) over a
  // tapered body reads as a bucket, distinct from a trash can.
  S3: '<ellipse cx="12" cy="7.3" rx="6.6" ry="2"/><path d="M5.5 7.8 7.2 18.7a1.5 1.5 0 0 0 1.48 1.3h6.64a1.5 1.5 0 0 0 1.48-1.3L18.5 7.8"/>',
  // Insights → line chart (Container Insights / metrics)
  Insights: '<path d="M4 4v16h16"/><path d="M7 15l3.5-4 3 2.5L20 7"/>',
};

// Returns inline SVG markup for a service, or "" if there is no icon for it.
export function iconSVG(svc) {
  const body = ICONS[svc];
  if (!body) return "";
  return `<svg class="svcicon" viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor"`
    + ` stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>`;
}
