// awsobs — a single-binary AWS + EKS observability tool with two modes:
//
//	awsobs server -config config.json   # collectors + dashboard (default)
//	awsobs agent  -config agent.json    # push host metrics/logs to a server
//
// The server collects CloudWatch metrics for managed services, native
// metrics straight from Valkey/OpenSearch/RabbitMQ endpoints (free,
// in-VPC), live pod/node metrics + logs from EKS, and receives pushes
// from agents. Agents run on EC2 instances or as an EKS DaemonSet.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/awsobs/internal/agent"
	"github.com/example/awsobs/internal/auth"
	"github.com/example/awsobs/internal/awsmetrics"
	"github.com/example/awsobs/internal/config"
	"github.com/example/awsobs/internal/db"
	"github.com/example/awsobs/internal/k8s"
	"github.com/example/awsobs/internal/logstore"
	"github.com/example/awsobs/internal/native"
	"github.com/example/awsobs/internal/server"
	"github.com/example/awsobs/internal/store"
	"github.com/example/awsobs/web"
)

func main() {
	mode := "server"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "server" || args[0] == "agent") {
		mode = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("awsobs "+mode, flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json (optional)")
	fs.Parse(args)

	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "agent":
		if err := agent.Run(ctx, cfg.Agent, logger); err != nil {
			logger.Fatal(err)
		}
	default:
		runServer(ctx, cfg, logger)
	}
}

func runServer(ctx context.Context, cfg config.Config, logger *log.Logger) {
	st := store.New(cfg.RetentionCap)
	ls := logstore.New(cfg.LogRetentionLines)
	inv := k8s.NewInventory()

	// SQLite persistence (optional): ensure schema, hydrate the hot stores
	// from disk so the dashboard serves data immediately, then persist all
	// polled data in the background while collectors refresh it.
	var sdb *db.DB
	var persistDone <-chan struct{}
	if cfg.DataDir != "" {
		var err error
		sdb, err = db.Open(cfg.DataDir, logger)
		if err != nil {
			logger.Printf("db: disabled: %v", err)
		} else {
			if cfg.DBRetentionHours > 0 {
				sdb.RetentionHours = cfg.DBRetentionHours
			}
			sdb.LogLinesPerSource = cfg.LogRetentionLines
			sdb.Hydrate(st, ls, inv)
			persistDone = sdb.StartPersist(ctx, st, ls, inv)
		}
	}

	// CloudWatch collector — degrades gracefully without credentials.
	var hist server.Historian
	var awsCol *awsmetrics.Collector
	if cfg.AWS.Enabled {
		col, err := awsmetrics.New(ctx, cfg.AWS, st, logger)
		if err != nil {
			logger.Printf("aws: collector disabled: %v", err)
		} else {
			go col.Run(ctx)
			hist = col
			awsCol = col
			logger.Printf("aws: collector started (region=%q profile=%q poll=%s)",
				cfg.AWS.Region, cfg.AWS.Profile, cfg.AWS.PollInterval())
		}
	}

	// Native pollers: Valkey / OpenSearch / RabbitMQ over their own APIs.
	go native.Run(ctx, cfg.Native, st, logger)

	// Kubernetes collectors + log streaming clients, one per cluster.
	var clusters []server.Cluster
	if cfg.Kubernetes.Enabled {
		for _, cc := range resolveClusters(ctx, cfg.Kubernetes, logger) {
			kc, err := k8s.NewClient(cc)
			if err != nil {
				logger.Printf("k8s(%s): client disabled: %v", cc.Name, err)
				continue
			}
			clusters = append(clusters, server.Cluster{Name: cc.Name, Client: kc})
			go k8s.NewCollector(cfg.Kubernetes, cc.Name, kc, st, inv, logger).Run(ctx)
			logger.Printf("k8s(%s): collector started (poll=%s)", cc.Name, cfg.Kubernetes.PollInterval())
		}
	}

	if cfg.IngestToken == "" {
		logger.Printf("WARNING: ingest_token is empty — /api/ingest is unauthenticated")
	}

	// Authentication (optional, enabled by default): a separate SQLite
	// user/session db seeded with admin/admin (must-change) on first setup.
	// Fail closed — if auth is enabled but can't start, don't silently serve
	// an unauthenticated dashboard.
	var authn server.Authenticator
	if cfg.Auth.On() {
		as, err := auth.Open(cfg.AuthDBPath(), logger)
		if err != nil {
			logger.Fatalf("auth: %v", err)
		}
		defer as.Close()
		authn = as
	} else {
		logger.Printf("auth: disabled — dashboard served without a login (set auth.enabled=true to require login)")
	}

	statusFn := func() map[string]any {
		out := map[string]any{}
		if awsCol != nil {
			a := awsCol.Status()
			a["namespaces"] = awsmetrics.EffectiveNamespaces(cfg.AWS)
			out["aws"] = a
		}
		native := []string{}
		if len(cfg.Native.Valkey) > 0 {
			native = append(native, "Valkey")
		}
		if len(cfg.Native.OpenSearch) > 0 {
			native = append(native, "OpenSearch")
		}
		if len(cfg.Native.RabbitMQ) > 0 {
			native = append(native, "MQ")
		}
		out["native"] = native
		return out
	}
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: server.New(st, ls, inv, clusters, hist, statusFn, cfg.IngestToken, authn, web.FS, logger),
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	logger.Printf("dashboard listening on http://localhost%s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
	if sdb != nil {
		select { // wait for the persister's final flush
		case <-persistDone:
		case <-time.After(3 * time.Second):
		}
		sdb.Close()
	}
	fmt.Fprintln(os.Stderr, "bye")
}

// resolveClusters turns config into concrete cluster endpoints:
// kubeconfig contexts (awsobs runs kubectl proxy for each), plus any
// directly configured clusters, falling back to legacy single-cluster
// config when neither is set.
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
		out = kcfg.ClusterList() // legacy: in-cluster or proxy on :8001
	}
	return out
}
