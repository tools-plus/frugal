// awsobs — a single-binary AWS + EKS observability tool.
//
//	go run ./cmd/awsobs -config config.json
//
// Collects CloudWatch metrics for managed services (EC2, RDS, ElastiCache/
// Valkey, AmazonMQ, OpenSearch, S3, ALB, NLB) and live pod/node CPU + memory
// plus log tails from an EKS cluster, then serves a live dashboard.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/awsobs/internal/awsmetrics"
	"github.com/example/awsobs/internal/config"
	"github.com/example/awsobs/internal/k8s"
	"github.com/example/awsobs/internal/server"
	"github.com/example/awsobs/internal/store"
	"github.com/example/awsobs/web"
)

func main() {
	configPath := flag.String("config", "", "path to config.json (optional)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st := store.New(cfg.RetentionCap)

	// CloudWatch collector — degrades gracefully if credentials are absent
	// so you can develop the EKS side without an AWS account wired up.
	var hist server.Historian
	if cfg.AWS.Enabled {
		col, err := awsmetrics.New(ctx, cfg.AWS, st, logger)
		if err != nil {
			logger.Printf("aws: collector disabled: %v", err)
		} else {
			go col.Run(ctx)
			hist = col
			logger.Printf("aws: collector started (region=%q profile=%q poll=%s)", cfg.AWS.Region, cfg.AWS.Profile, cfg.AWS.PollInterval())
		}
	}

	// Kubernetes collector + log streaming client.
	var kc *k8s.Client
	if cfg.Kubernetes.Enabled {
		kc, err = k8s.NewClient(cfg.Kubernetes)
		if err != nil {
			logger.Printf("k8s: client disabled: %v", err)
			kc = nil
		} else {
			go k8s.NewCollector(cfg.Kubernetes, kc, st, logger).Run(ctx)
			logger.Printf("k8s: collector started (poll=%s)", cfg.Kubernetes.PollInterval())
		}
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: server.New(st, kc, hist, web.FS, logger),
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	logger.Printf("dashboard listening on http://localhost%s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
}
