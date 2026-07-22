// Package native polls managed services over their own free, in-VPC
// endpoints instead of CloudWatch: Valkey/Redis INFO, OpenSearch stats
// APIs, and the RabbitMQ management API. Same store, same dashboard,
// zero CloudWatch cost, second-level resolution.
package native

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tools-plus/awsobs/internal/config"
	"github.com/tools-plus/awsobs/internal/store"
)

// ---------------------------------------------------------------- Valkey

// valkeyPoller speaks just enough RESP to run INFO — no client library.
type valkeyPoller struct {
	cfg   config.ValkeyTarget
	store *store.Store
	log   *log.Logger

	prev     map[string]float64 // previous counter values for rate calc
	prevTime time.Time
}

// counters are monotonically increasing INFO fields we convert to per-second
// rates; gauges are stored as-is.
var valkeyCounters = map[string]string{
	"keyspace_hits":            "keyspace_hits_per_sec",
	"keyspace_misses":          "keyspace_misses_per_sec",
	"evicted_keys":             "evictions_per_sec",
	"expired_keys":             "expirations_per_sec",
	"total_net_input_bytes":    "net_in_bytes_per_sec",
	"total_net_output_bytes":   "net_out_bytes_per_sec",
	"total_commands_processed": "commands_per_sec",
}

var valkeyGauges = map[string]string{
	"used_memory":               "used_memory_bytes",
	"maxmemory":                 "maxmemory_bytes",
	"connected_clients":         "connected_clients",
	"blocked_clients":           "blocked_clients",
	"instantaneous_ops_per_sec": "ops_per_sec_instant",
	"mem_fragmentation_ratio":   "mem_fragmentation_ratio",
}

func (p *valkeyPoller) poll(ctx context.Context) error {
	info, err := valkeyInfo(ctx, p.cfg)
	if err != nil {
		return err
	}
	now := time.Now()
	ts := now.Unix()

	emit := func(metric string, v float64) {
		p.store.Add("nv|valkey|"+p.cfg.Name+"|"+metric, map[string]string{
			"source": "native", "svc": "Valkey",
			"resource": p.cfg.Name, "metric": metric, "variant": "",
		}, store.Point{T: ts, V: v})
	}

	for field, metric := range valkeyGauges {
		if v, ok := parseF(info[field]); ok {
			emit(metric, v)
		}
	}
	// memory pct if maxmemory known
	if used, ok1 := parseF(info["used_memory"]); ok1 {
		if max, ok2 := parseF(info["maxmemory"]); ok2 && max > 0 {
			emit("memory_used_pct", used/max*100)
		}
	}
	// counter rates
	elapsed := now.Sub(p.prevTime).Seconds()
	cur := map[string]float64{}
	for field, metric := range valkeyCounters {
		v, ok := parseF(info[field])
		if !ok {
			continue
		}
		cur[field] = v
		if prev, ok := p.prev[field]; ok && elapsed > 0 && v >= prev {
			emit(metric, (v-prev)/elapsed)
		}
	}
	// engine CPU pct from used_cpu deltas
	if sys, ok1 := parseF(info["used_cpu_sys"]); ok1 {
		if usr, ok2 := parseF(info["used_cpu_user"]); ok2 {
			total := sys + usr
			cur["_cpu"] = total
			if prev, ok := p.prev["_cpu"]; ok && elapsed > 0 && total >= prev {
				emit("engine_cpu_pct", (total-prev)/elapsed*100)
			}
		}
	}
	p.prev, p.prevTime = cur, now
	return nil
}

func valkeyInfo(ctx context.Context, t config.ValkeyTarget) (map[string]string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	var (
		conn net.Conn
		err  error
	)
	if t.TLS {
		conn, err = tls.DialWithDialer(&d, "tcp", t.Addr, &tls.Config{InsecureSkipVerify: t.InsecureSkipTLSVerify})
	} else {
		conn, err = d.DialContext(ctx, "tcp", t.Addr)
	}
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)

	if t.Password != "" {
		if err := respCmd(conn, r, nil, "AUTH", t.Password); err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	var payload string
	if err := respCmd(conn, r, &payload, "INFO"); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, ':'); i > 0 && !strings.HasPrefix(line, "#") {
			out[line[:i]] = line[i+1:]
		}
	}
	return out, nil
}

// respCmd sends a RESP array command and reads one reply. If bulk is
// non-nil the reply must be a bulk string and is stored there.
func respCmd(conn net.Conn, r *bufio.Reader, bulk *string, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "-"):
		return fmt.Errorf("server error: %s", line[1:])
	case strings.HasPrefix(line, "+"):
		return nil
	case strings.HasPrefix(line, "$"):
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return fmt.Errorf("bad bulk length %q", line)
		}
		buf := make([]byte, n+2) // payload + CRLF
		if _, err := ioReadFull(r, buf); err != nil {
			return err
		}
		if bulk != nil {
			*bulk = string(buf[:n])
		}
		return nil
	default:
		return fmt.Errorf("unexpected reply %q", line)
	}
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func parseF(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}
