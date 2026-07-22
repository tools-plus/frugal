// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package native

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tools-plus/frugal/internal/config"
	"github.com/tools-plus/frugal/internal/store"
)

func httpGetJSON(ctx context.Context, url, user, pass string, insecure bool, out any) error {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}
	cl := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ------------------------------------------------------------ OpenSearch

type osPoller struct {
	cfg   config.OpenSearchTarget
	store *store.Store
}

func (p *osPoller) poll(ctx context.Context) error {
	ts := time.Now().Unix()
	emit := func(metric, variant string, v float64) {
		id := "nv|opensearch|" + p.cfg.Name + "|" + metric
		if variant != "" {
			id += "|" + variant
		}
		p.store.Add(id, map[string]string{
			"source": "native", "svc": "OpenSearch",
			"resource": p.cfg.Name, "metric": metric, "variant": variant,
		}, store.Point{T: ts, V: v})
	}

	var health struct {
		Status           string  `json:"status"`
		NumberOfNodes    float64 `json:"number_of_nodes"`
		ActiveShards     float64 `json:"active_shards"`
		UnassignedShards float64 `json:"unassigned_shards"`
		PendingTasks     float64 `json:"number_of_pending_tasks"`
	}
	if err := httpGetJSON(ctx, p.cfg.URL+"/_cluster/health", p.cfg.Username, p.cfg.Password, p.cfg.Insecure, &health); err != nil {
		return err
	}
	statusV := map[string]float64{"green": 0, "yellow": 1, "red": 2}[health.Status]
	emit("cluster_status", "", statusV)
	emit("nodes", "", health.NumberOfNodes)
	emit("active_shards", "", health.ActiveShards)
	emit("unassigned_shards", "", health.UnassignedShards)
	emit("pending_tasks", "", health.PendingTasks)

	var stats struct {
		Nodes map[string]struct {
			Name string `json:"name"`
			JVM  struct {
				Mem struct {
					HeapUsedPercent float64 `json:"heap_used_percent"`
				} `json:"mem"`
			} `json:"jvm"`
			OS struct {
				CPU struct {
					Percent float64 `json:"percent"`
				} `json:"cpu"`
			} `json:"os"`
			FS struct {
				Total struct {
					AvailableInBytes float64 `json:"available_in_bytes"`
				} `json:"total"`
			} `json:"fs"`
		} `json:"nodes"`
	}
	if err := httpGetJSON(ctx, p.cfg.URL+"/_nodes/stats/jvm,os,fs", p.cfg.Username, p.cfg.Password, p.cfg.Insecure, &stats); err != nil {
		return err
	}
	for _, n := range stats.Nodes {
		emit("jvm_heap_pct", n.Name, n.JVM.Mem.HeapUsedPercent)
		emit("cpu_pct", n.Name, n.OS.CPU.Percent)
		emit("fs_available_bytes", n.Name, n.FS.Total.AvailableInBytes)
	}
	return nil
}

// -------------------------------------------------------------- RabbitMQ

type rabbitPoller struct {
	cfg   config.RabbitTarget
	store *store.Store
}

const rabbitQueueCap = 50 // per-broker cap on per-queue series

func (p *rabbitPoller) poll(ctx context.Context) error {
	ts := time.Now().Unix()
	emit := func(metric, variant string, v float64) {
		id := "nv|rabbitmq|" + p.cfg.Name + "|" + metric
		if variant != "" {
			id += "|" + variant
		}
		p.store.Add(id, map[string]string{
			"source": "native", "svc": "MQ",
			"resource": p.cfg.Name, "metric": metric, "variant": variant,
		}, store.Point{T: ts, V: v})
	}

	var ov struct {
		QueueTotals struct {
			Messages      float64 `json:"messages"`
			MessagesReady float64 `json:"messages_ready"`
			MessagesUnack float64 `json:"messages_unacknowledged"`
		} `json:"queue_totals"`
		ObjectTotals struct {
			Connections float64 `json:"connections"`
			Queues      float64 `json:"queues"`
			Consumers   float64 `json:"consumers"`
		} `json:"object_totals"`
		MessageStats struct {
			PublishDetails struct {
				Rate float64 `json:"rate"`
			} `json:"publish_details"`
			AckDetails struct {
				Rate float64 `json:"rate"`
			} `json:"ack_details"`
		} `json:"message_stats"`
	}
	if err := httpGetJSON(ctx, p.cfg.URL+"/api/overview", p.cfg.Username, p.cfg.Password, p.cfg.Insecure, &ov); err != nil {
		return err
	}
	emit("messages_total", "", ov.QueueTotals.Messages)
	emit("messages_ready", "", ov.QueueTotals.MessagesReady)
	emit("messages_unacked", "", ov.QueueTotals.MessagesUnack)
	emit("connections", "", ov.ObjectTotals.Connections)
	emit("queues", "", ov.ObjectTotals.Queues)
	emit("consumers", "", ov.ObjectTotals.Consumers)
	emit("publish_per_sec", "", ov.MessageStats.PublishDetails.Rate)
	emit("ack_per_sec", "", ov.MessageStats.AckDetails.Rate)

	var queues []struct {
		Name      string  `json:"name"`
		Messages  float64 `json:"messages"`
		Consumers float64 `json:"consumers"`
	}
	if err := httpGetJSON(ctx, p.cfg.URL+"/api/queues?page=1&page_size=50&sort=messages&sort_reverse=true", p.cfg.Username, p.cfg.Password, p.cfg.Insecure, &queues); err == nil {
		for i, q := range queues {
			if i >= rabbitQueueCap {
				break
			}
			emit("queue_depth", q.Name, q.Messages)
			emit("queue_consumers", q.Name, q.Consumers)
		}
	}
	return nil
}

// ---------------------------------------------------------------- Runner

type poller interface {
	poll(ctx context.Context) error
}

type named struct {
	name string
	p    poller
}

// Run schedules all configured native pollers on one ticker. Each poll is
// independent — one unreachable endpoint doesn't block the others.
func Run(ctx context.Context, cfg config.NativeConfig, st *store.Store, logger Logger) {
	var pollers []named
	for _, t := range cfg.Valkey {
		pollers = append(pollers, named{"valkey/" + t.Name, &valkeyPoller{cfg: t, store: st}})
	}
	for _, t := range cfg.OpenSearch {
		pollers = append(pollers, named{"opensearch/" + t.Name, &osPoller{cfg: t, store: st}})
	}
	for _, t := range cfg.RabbitMQ {
		pollers = append(pollers, named{"rabbitmq/" + t.Name, &rabbitPoller{cfg: t, store: st}})
	}
	if len(pollers) == 0 {
		return
	}
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 15 * time.Second
	}
	logger.Printf("native: %d pollers started (interval=%s)", len(pollers), interval)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	pollAll := func() {
		for _, n := range pollers {
			go func(n named) {
				if err := n.p.poll(ctx); err != nil {
					logger.Printf("native: %s: %v", n.name, err)
				}
			}(n)
		}
	}
	pollAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pollAll()
		}
	}
}

// Logger is the minimal logging interface we need (satisfied by *log.Logger).
type Logger interface {
	Printf(format string, v ...any)
}
