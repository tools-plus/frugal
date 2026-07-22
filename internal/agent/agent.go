// Package agent implements `awsobs agent`: a small push-mode collector for
// EC2 instances and EKS nodes (as a DaemonSet). It reads host metrics from
// /proc, tails log files (including /var/log/containers/*.log on Kubernetes
// nodes), and ships everything to the awsobs server's ingest API.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tools-plus/awsobs/internal/config"
	"github.com/tools-plus/awsobs/internal/store"
)

// IngestPoint mirrors the server's ingest wire format.
type IngestPoint struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	Point  store.Point       `json:"point"`
}

type IngestBatch struct {
	Batch []IngestPoint `json:"batch"`
}

type IngestLogs struct {
	Source string   `json:"source"`
	Lines  []string `json:"lines"`
}

type Agent struct {
	cfg    config.AgentConfig
	logger *log.Logger
	host   string
	client *http.Client

	prevCPU  []float64
	prevNet  [2]float64
	prevTime time.Time

	queue   []IngestPoint // buffered on push failure
	tailers map[string]*tailer
}

func Run(ctx context.Context, cfg config.AgentConfig, logger *log.Logger) error {
	if cfg.ServerURL == "" {
		return fmt.Errorf("agent: server_url required (config agent.server_url or AWSOBS_SERVER_URL)")
	}
	host := cfg.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	a := &Agent{
		cfg: cfg, logger: logger, host: host,
		client:  &http.Client{Timeout: 10 * time.Second},
		tailers: map[string]*tailer{},
	}
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 15 * time.Second
	}
	logger.Printf("agent: %s -> %s every %s (log globs: %v, kube logs: %v)",
		host, cfg.ServerURL, interval, cfg.LogGlobs, cfg.KubeLogs)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		a.cycle(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (a *Agent) cycle(ctx context.Context) {
	pts := a.hostMetrics()
	a.queue = append(a.queue, pts...)
	if len(a.queue) > 10000 {
		a.queue = a.queue[len(a.queue)-10000:] // drop oldest on prolonged outage
	}
	if err := a.post(ctx, "/api/ingest", IngestBatch{Batch: a.queue}); err != nil {
		a.logger.Printf("agent: push failed (buffering %d points): %v", len(a.queue), err)
	} else {
		a.queue = a.queue[:0]
	}
	a.shipLogs(ctx)
}

func (a *Agent) post(ctx context.Context, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.cfg.ServerURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return nil
}

// ------------------------------------------------------- host metrics

func (a *Agent) hostMetrics() []IngestPoint {
	now := time.Now()
	ts := now.Unix()
	var pts []IngestPoint
	emit := func(metric string, v float64) {
		pts = append(pts, IngestPoint{
			ID: "ag|host|" + a.host + "|" + metric,
			Labels: map[string]string{
				"source": "agent", "kind": "host",
				"resource": a.host, "metric": metric, "variant": "",
			},
			Point: store.Point{T: ts, V: v},
		})
	}

	// CPU % from /proc/stat deltas
	if cur, err := readCPUStat(); err == nil {
		if a.prevCPU != nil {
			var dTotal, dIdle float64
			for i := range cur {
				prev := 0.0
				if i < len(a.prevCPU) {
					prev = a.prevCPU[i]
				}
				dTotal += cur[i] - prev
				if i == 3 || i == 4 { // idle, iowait
					dIdle += cur[i] - prev
				}
			}
			if dTotal > 0 {
				emit("cpu_pct", (1-dIdle/dTotal)*100)
			}
		}
		a.prevCPU = cur
	}

	// Memory from /proc/meminfo
	if mi, err := readMeminfo(); err == nil {
		total, avail := mi["MemTotal"], mi["MemAvailable"]
		if total > 0 {
			emit("memory_used_bytes", total-avail)
			emit("memory_total_bytes", total)
			emit("memory_used_pct", (total-avail)/total*100)
		}
	}

	// Root filesystem
	var fs syscall.Statfs_t
	if err := syscall.Statfs("/", &fs); err == nil && fs.Blocks > 0 {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		emit("disk_used_pct", (total-free)/total*100)
		emit("disk_free_bytes", free)
	}

	// Network bytes/sec from /proc/net/dev deltas (all non-lo interfaces)
	if rx, tx, err := readNetDev(); err == nil {
		if !a.prevTime.IsZero() {
			el := now.Sub(a.prevTime).Seconds()
			if el > 0 && rx >= a.prevNet[0] && tx >= a.prevNet[1] {
				emit("net_rx_bytes_per_sec", (rx-a.prevNet[0])/el)
				emit("net_tx_bytes_per_sec", (tx-a.prevNet[1])/el)
			}
		}
		a.prevNet = [2]float64{rx, tx}
	}

	// Load average
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				emit("load1", v)
			}
		}
	}

	a.prevTime = now
	return pts
}

func readCPUStat() ([]float64, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line) // "cpu user nice system idle iowait irq softirq steal ..."
	if len(f) < 5 || f[0] != "cpu" {
		return nil, fmt.Errorf("unexpected /proc/stat")
	}
	out := make([]float64, 0, len(f)-1)
	for _, s := range f[1:] {
		v, _ := strconv.ParseFloat(s, 64)
		out = append(out, v)
	}
	return out, nil
}

func readMeminfo() (map[string]float64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			key := strings.TrimSuffix(f[0], ":")
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				out[key] = v * 1024 // meminfo is in kB
			}
		}
	}
	return out, nil
}

func readNetDev() (rx, tx float64, err error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 9 {
			r, _ := strconv.ParseFloat(f[0], 64)
			t, _ := strconv.ParseFloat(f[8], 64)
			rx += r
			tx += t
		}
	}
	return rx, tx, nil
}

// ---------------------------------------------------------- log tailing

type tailer struct {
	offset  int64
	partial string
}

func (a *Agent) shipLogs(ctx context.Context) {
	globs := append([]string{}, a.cfg.LogGlobs...)
	if a.cfg.KubeLogs {
		globs = append(globs, "/var/log/containers/*.log")
	}
	seen := map[string]bool{}
	for _, g := range globs {
		paths, _ := filepath.Glob(g)
		for _, p := range paths {
			seen[p] = true
			a.tailFile(ctx, p)
		}
	}
	for p := range a.tailers { // forget rotated-away files
		if !seen[p] {
			delete(a.tailers, p)
		}
	}
}

const maxReadPerCycle = 256 * 1024

func (a *Agent) tailFile(ctx context.Context, path string) {
	t, known := a.tailers[path]
	if !known {
		t = &tailer{}
		a.tailers[path] = t
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if !known {
		t.offset = fi.Size() // start at end for newly seen files
		return
	}
	if fi.Size() < t.offset { // rotation/truncation
		t.offset = 0
	}
	if fi.Size() == t.offset {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, 0); err != nil {
		return
	}
	n := fi.Size() - t.offset
	if n > maxReadPerCycle {
		n = maxReadPerCycle
	}
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	t.offset += int64(read)

	chunk := t.partial + string(buf[:read])
	lines := strings.Split(chunk, "\n")
	t.partial = lines[len(lines)-1] // may be an incomplete final line
	lines = lines[:len(lines)-1]
	if len(lines) == 0 {
		return
	}
	if len(lines) > 500 {
		lines = lines[len(lines)-500:]
	}
	src, prefix := a.sourceFor(path)
	if prefix != "" {
		for i := range lines {
			lines[i] = prefix + lines[i]
		}
	}
	if err := a.post(ctx, "/api/ingest/logs", IngestLogs{Source: src, Lines: lines}); err != nil {
		a.logger.Printf("agent: log ship failed for %s: %v", path, err)
		t.offset -= int64(read) // retry this chunk next cycle
		t.partial = ""
	}
}

// sourceFor maps a log path to a log-store source. Kubernetes container
// logs (<pod>_<namespace>_<container>-<id>.log) become "pod/<ns>/<pod>";
// everything else is grouped under "host/<hostname>" with a file prefix.
func (a *Agent) sourceFor(path string) (source, prefix string) {
	base := filepath.Base(path)
	if strings.HasPrefix(path, "/var/log/containers/") {
		parts := strings.SplitN(strings.TrimSuffix(base, ".log"), "_", 3)
		if len(parts) == 3 {
			return "pod/" + parts[1] + "/" + parts[0], ""
		}
	}
	return "host/" + a.host, "[" + base + "] "
}
