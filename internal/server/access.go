package server

import (
	"net/http"
	"strings"
)

// ns2svc maps CloudWatch namespaces (minus the "AWS/" prefix) to the dashboard
// service keys. Mirrors NS2SVC in web/js/state.js — keep the two in sync.
var ns2svc = map[string]string{
	"EC2": "EC2", "RDS": "RDS", "DocDB": "DocDB", "ElastiCache": "Valkey",
	"AmazonMQ": "MQ", "ES": "OpenSearch", "S3": "S3", "ApplicationELB": "ALB",
	"NetworkELB": "NLB", "EKS": "EKS", "ContainerInsights": "Insights",
}

// serviceOf classifies a series/update by its labels into a service key, the
// same taxonomy the rail uses. Mirrors svcOf() in web/js/state.js.
func serviceOf(labels map[string]string) string {
	switch labels["source"] {
	case "k8s":
		return "EKS"
	case "agent":
		return "Hosts"
	case "native":
		if s := labels["svc"]; s != "" {
			return s
		}
		return "Native"
	}
	ns := strings.TrimPrefix(labels["namespace"], "AWS/")
	if s, ok := ns2svc[ns]; ok {
		return s
	}
	if ns != "" {
		return ns
	}
	return "Other"
}

// access resolves the request's user into (isAdmin, allow) where allow(service)
// reports whether they may see that service. With auth disabled, everything is
// allowed. Callers are gated handlers, so a valid session is expected; a
// missing one denies everything defensively.
func (s *Server) access(r *http.Request) (isAdmin bool, allow func(string) bool) {
	if !s.authEnabled {
		return true, func(string) bool { return true }
	}
	user, ok := s.sessionUser(r)
	if !ok {
		return false, func(string) bool { return false }
	}
	admin, services := s.authn.UserAccess(user)
	if admin {
		return true, func(string) bool { return true }
	}
	all := false
	set := make(map[string]bool, len(services))
	for _, sv := range services {
		if sv == "*" {
			all = true
		}
		set[sv] = true
	}
	return false, func(svc string) bool { return all || set[svc] }
}
