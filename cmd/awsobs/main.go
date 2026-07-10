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

	"github.com/example/awsobs/internal/agent"
	"github.com/example/awsobs/internal/awsmetrics"
	"github.com/example/awsobs/internal/config"
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

	// CloudWatch collector — degrades gracefully without credentials.
	var hist server.Historian
	if cfg.AWS.Enabled {
		col, err := awsmetrics.New(ctx, cfg.AWS, st, logger)
		if err != nil {
			logger.Printf("aws: collector disabled: %v", err)
		} else {
			go col.Run(ctx)
			hist = col
			logger.Printf("aws: collector started (region=%q profile=%q poll=%s)",
				cfg.AWS.Region, cfg.AWS.Profile, cfg.AWS.PollInterval())
		}
	}

	// Native pollers: Valkey / OpenSearch / RabbitMQ over their own APIs.
	go native.Run(ctx, cfg.Native, st, logger)

	// Kubernetes collector + log streaming client.
	var kc *k8s.Client
	if cfg.Kubernetes.Enabled {
		var err error
		kc, err = k8s.NewClient(cfg.Kubernetes)
		if err != nil {
			logger.Printf("k8s: client disabled: %v", err)
			kc = nil
		} else {
			go k8s.NewCollector(cfg.Kubernetes, kc, st, logger).Run(ctx)
			logger.Printf("k8s: collector started (poll=%s)", cfg.Kubernetes.PollInterval())
		}
	}

	if cfg.IngestToken == "" {
		logger.Printf("WARNING: ingest_token is empty — /api/ingest is unauthenticated")
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: server.New(st, ls, kc, hist, cfg.IngestToken, web.FS, logger),
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	logger.Printf("dashboard listening on http://localhost%s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "bye")
}
