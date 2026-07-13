// Package collector is the supervised data-collection service ("Part 2"): it
// starts the AWS, native, and Kubernetes collectors from a runtime config and
// can tear them all down and relaunch with new config — without restarting the
// web server ("Part 1"). Each generation runs under its own context, so a
// reconfigure cancels the AWS/native/k8s goroutines and kills their kubectl
// proxies (started via exec.CommandContext) cleanly.
package collector

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/example/awsobs/internal/awsmetrics"
	"github.com/example/awsobs/internal/config"
	"github.com/example/awsobs/internal/k8s"
	"github.com/example/awsobs/internal/logstore"
	"github.com/example/awsobs/internal/native"
	"github.com/example/awsobs/internal/store"
)

// Cluster pairs a resolved cluster name with its API client (for log streaming).
type Cluster struct {
	Name   string
	Client *k8s.Client
}

// Supervisor owns the current generation of collectors and swaps it on Apply.
type Supervisor struct {
	appCtx context.Context
	st     *store.Store
	ls     *logstore.Store
	inv    *k8s.Inventory
	logger *log.Logger

	mu         sync.Mutex
	cancel     context.CancelFunc
	wg         *sync.WaitGroup
	rt         config.Runtime
	clusters   []Cluster
	aws        *awsmetrics.Collector
	nativeSvcs []string
}

func New(appCtx context.Context, st *store.Store, ls *logstore.Store, inv *k8s.Inventory, logger *log.Logger) *Supervisor {
	return &Supervisor{appCtx: appCtx, st: st, ls: ls, inv: inv, logger: logger}
}

// Apply tears down the running collectors and starts a fresh generation from
// rt. Safe to call concurrently with the accessors below.
func (s *Supervisor) Apply(rt config.Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.teardownLocked()

	ctx, cancel := context.WithCancel(s.appCtx)
	s.cancel = cancel
	s.wg = &sync.WaitGroup{}
	s.rt = rt
	s.clusters = nil
	s.aws = nil
	s.nativeSvcs = nil

	run := func(f func()) { s.wg.Add(1); go func() { defer s.wg.Done(); f() }() }

	// AWS — only when enabled and credentials resolve.
	if rt.AWS.Enabled {
		if col, err := awsmetrics.New(ctx, rt.AWS, s.st, s.logger); err != nil {
			s.logger.Printf("aws: collector not started: %v", err)
		} else {
			s.aws = col
			run(func() { col.Run(ctx) })
			s.logger.Printf("aws: collector started (region=%q poll=%s)", rt.AWS.Region, rt.AWS.PollInterval())
		}
	}

	// Native pollers — only when targets are configured.
	if n := len(rt.Native.Valkey) + len(rt.Native.OpenSearch) + len(rt.Native.RabbitMQ); n > 0 {
		if len(rt.Native.Valkey) > 0 {
			s.nativeSvcs = append(s.nativeSvcs, "Valkey")
		}
		if len(rt.Native.OpenSearch) > 0 {
			s.nativeSvcs = append(s.nativeSvcs, "OpenSearch")
		}
		if len(rt.Native.RabbitMQ) > 0 {
			s.nativeSvcs = append(s.nativeSvcs, "MQ")
		}
		run(func() { native.Run(ctx, rt.Native, s.st, s.logger) })
		s.logger.Printf("native: %d target(s) started", n)
	}

	// Kubernetes — one collector per resolved cluster.
	if rt.Kubernetes.Enabled {
		for _, cc := range resolveClusters(ctx, rt.Kubernetes, s.logger) {
			kc, err := k8s.NewClient(cc)
			if err != nil {
				s.logger.Printf("k8s(%s): client disabled: %v", cc.Name, err)
				continue
			}
			s.clusters = append(s.clusters, Cluster{Name: cc.Name, Client: kc})
			col := k8s.NewCollector(rt.Kubernetes, cc.Name, kc, s.st, s.inv, s.logger)
			run(func() { col.Run(ctx) })
			s.logger.Printf("k8s(%s): collector started (poll=%s)", cc.Name, rt.Kubernetes.PollInterval())
		}
	}
}

// teardownLocked cancels the current generation and waits (bounded) for it to
// stop. Caller holds s.mu.
func (s *Supervisor) teardownLocked() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.logger.Printf("collector: teardown timed out after 5s (continuing)")
	}
	s.cancel = nil
}

// Close stops all collectors (called on shutdown).
func (s *Supervisor) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardownLocked()
}

// AWSCollector returns the running AWS collector, or nil when it isn't running.
func (s *Supervisor) AWSCollector() *awsmetrics.Collector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aws
}

// Clusters returns the currently resolved clusters.
func (s *Supervisor) Clusters() []Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Cluster, len(s.clusters))
	copy(out, s.clusters)
	return out
}

// Status reports collector health for /api/status.
func (s *Supervisor) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	if s.aws != nil {
		a := s.aws.Status()
		a["namespaces"] = awsmetrics.EffectiveNamespaces(s.rt.AWS)
		out["aws"] = a
	}
	if s.nativeSvcs == nil {
		s.nativeSvcs = []string{}
	}
	out["native"] = s.nativeSvcs
	return out
}

// resolveClusters turns k8s config into concrete cluster endpoints: kubeconfig
// contexts (a supervised kubectl proxy each), plus directly configured
// clusters, falling back to legacy single-cluster config.
func resolveClusters(ctx context.Context, kcfg config.K8sConfig, logger *log.Logger) []config.ClusterConfig {
	var out []config.ClusterConfig
	seen := map[string]bool{}
	add := func(cc config.ClusterConfig) {
		if !seen[cc.Name] {
			seen[cc.Name] = true
			out = append(out, cc)
		}
	}
	if len(kcfg.Contexts) > 0 {
		names, err := k8s.ExpandContexts(ctx, kcfg.Contexts)
		if err != nil {
			logger.Printf("k8s: context discovery failed: %v", err)
		}
		for _, cn := range names {
			url, err := k8s.StartProxy(ctx, cn, logger)
			if err != nil {
				logger.Printf("k8s: %v", err)
				continue
			}
			name := k8s.ContextDisplayName(cn)
			logger.Printf("k8s(%s): kubectl proxy started at %s (context %s)", name, url, cn)
			add(config.ClusterConfig{Name: name, APIURL: url})
		}
	}
	for _, cc := range kcfg.Clusters {
		add(cc)
	}
	if len(out) == 0 {
		out = kcfg.ClusterList()
	}
	return out
}
