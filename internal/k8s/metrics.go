package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tools-plus/awsobs/internal/config"
	"github.com/tools-plus/awsobs/internal/store"
)

// Collector polls the metrics.k8s.io API (backed by metrics-server, which
// EKS installs via the metrics-server addon) for live pod and node CPU and
// memory usage.
type Collector struct {
	cfg     config.K8sConfig
	cluster string
	client  *Client
	store   *store.Store
	logger  *log.Logger

	inv *Inventory
	wl  map[string]PodInfo // "ns/pod" -> inventory (for workload labels)
}

func NewCollector(cfg config.K8sConfig, cluster string, cl *Client, st *store.Store, inv *Inventory, logger *log.Logger) *Collector {
	return &Collector{cfg: cfg, cluster: cluster, client: cl, store: st, inv: inv, logger: logger, wl: map[string]PodInfo{}}
}

func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.PollInterval())
	defer t.Stop()
	c.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.poll(ctx)
		}
	}
}

// ---- metrics.k8s.io response shapes (only the fields we need) ----

type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Timestamp  time.Time `json:"timestamp"`
		Containers []struct {
			Name  string `json:"name"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

type nodeMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Timestamp time.Time `json:"timestamp"`
		Usage     struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"usage"`
	} `json:"items"`
}

func (c *Collector) poll(ctx context.Context) {
	// Refresh the pod inventory first so metric series get workload labels.
	if pods, err := c.client.ListPods(ctx, ""); err == nil {
		m := make(map[string]PodInfo, len(pods))
		for _, p := range pods {
			m[p.Namespace+"/"+p.Name] = p
		}
		c.wl = m
		if c.inv != nil {
			c.inv.Set(c.cluster, pods)
		}
	} else {
		c.logger.Printf("k8s(%s): pod inventory: %v", c.cluster, err)
	}
	c.pollNodes(ctx)
	c.pollPods(ctx)
}

func (c *Collector) pollNodes(ctx context.Context) {
	b, err := c.client.GetJSON(ctx, "/apis/metrics.k8s.io/v1beta1/nodes")
	if err != nil {
		c.logger.Printf("k8s: node metrics: %v", err)
		return
	}
	var list nodeMetricsList
	if err := json.Unmarshal(b, &list); err != nil {
		c.logger.Printf("k8s: node metrics decode: %v", err)
		return
	}
	for _, n := range list.Items {
		ts := n.Timestamp.Unix()
		base := map[string]string{
			"source": "k8s", "kind": "node", "cluster": c.cluster,
			"node": n.Metadata.Name, "resource": n.Metadata.Name,
		}
		if cpu, err := ParseCPU(n.Usage.CPU); err == nil {
			c.store.Add("k8s|"+c.cluster+"|node|"+n.Metadata.Name+"|cpu_cores",
				cloneWith(base, "metric", "cpu_cores"), store.Point{T: ts, V: cpu})
		}
		if mem, err := ParseMemory(n.Usage.Memory); err == nil {
			c.store.Add("k8s|"+c.cluster+"|node|"+n.Metadata.Name+"|memory_bytes",
				cloneWith(base, "metric", "memory_bytes"), store.Point{T: ts, V: mem})
		}
	}
}

func (c *Collector) pollPods(ctx context.Context) {
	paths := []string{"/apis/metrics.k8s.io/v1beta1/pods"}
	if len(c.cfg.Namespaces) > 0 {
		paths = paths[:0]
		for _, ns := range c.cfg.Namespaces {
			paths = append(paths, "/apis/metrics.k8s.io/v1beta1/namespaces/"+url.PathEscape(ns)+"/pods")
		}
	}
	for _, p := range paths {
		b, err := c.client.GetJSON(ctx, p)
		if err != nil {
			c.logger.Printf("k8s: pod metrics: %v", err)
			continue
		}
		var list podMetricsList
		if err := json.Unmarshal(b, &list); err != nil {
			c.logger.Printf("k8s: pod metrics decode: %v", err)
			continue
		}
		for _, pod := range list.Items {
			ts := pod.Timestamp.Unix()
			key := pod.Metadata.Namespace + "/" + pod.Metadata.Name
			var cpu, mem float64
			for _, ct := range pod.Containers {
				if v, err := ParseCPU(ct.Usage.CPU); err == nil {
					cpu += v
				}
				if v, err := ParseMemory(ct.Usage.Memory); err == nil {
					mem += v
				}
			}
			labels := map[string]string{
				"source": "k8s", "kind": "pod", "resource": key, "cluster": c.cluster,
				"namespace": pod.Metadata.Namespace, "pod": pod.Metadata.Name,
			}
			if info, ok := c.wl[key]; ok {
				labels["workload"] = info.Workload
				labels["workload_kind"] = info.WorkloadKind
			}
			cl := cloneWith(labels, "metric", "cpu_cores")
			c.store.Add("k8s|"+c.cluster+"|pod|"+key+"|cpu_cores", cl, store.Point{T: ts, V: cpu})
			ml := cloneWith(labels, "metric", "memory_bytes")
			c.store.Add("k8s|"+c.cluster+"|pod|"+key+"|memory_bytes", ml, store.Point{T: ts, V: mem})
		}
	}
}

func cloneWith(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

// ---- Kubernetes resource.Quantity parsing (the subset kubelet emits) ----

// ParseCPU converts a CPU quantity ("250m", "1", "1500000n") to cores.
func ParseCPU(q string) (float64, error) {
	if q == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(q, "n"):
		mult, q = 1e-9, strings.TrimSuffix(q, "n")
	case strings.HasSuffix(q, "u"):
		mult, q = 1e-6, strings.TrimSuffix(q, "u")
	case strings.HasSuffix(q, "m"):
		mult, q = 1e-3, strings.TrimSuffix(q, "m")
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, err
	}
	return v * mult, nil
}

// ParseMemory converts a memory quantity ("128974848", "129Mi", "1Gi", "64k")
// to bytes.
func ParseMemory(q string) (float64, error) {
	if q == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	suffixes := []struct {
		s string
		m float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}
	mult := 1.0
	for _, sf := range suffixes {
		if strings.HasSuffix(q, sf.s) {
			mult = sf.m
			q = strings.TrimSuffix(q, sf.s)
			break
		}
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, err
	}
	return v * mult, nil
}
