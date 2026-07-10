// Package config loads awsobs configuration from a JSON file with
// environment-variable overrides. JSON keeps the binary dependency-free.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type K8sConfig struct {
	Enabled bool `json:"enabled"`
	// Namespaces to watch; empty means all namespaces.
	Namespaces []string `json:"namespaces"`
	// PollIntervalSeconds for pod/node metrics (metrics-server resolution is ~15s).
	PollIntervalSeconds int `json:"poll_interval_seconds"`
	// APIURL overrides autodetection (e.g. http://127.0.0.1:8001 for `kubectl proxy`).
	APIURL string `json:"api_url"`
	// BearerToken for out-of-cluster access to a real API server.
	BearerToken string `json:"bearer_token"`
	// InsecureSkipTLSVerify for lab clusters only.
	InsecureSkipTLSVerify bool `json:"insecure_skip_tls_verify"`
}

type AWSConfig struct {
	Enabled bool   `json:"enabled"`
	Region  string `json:"region"`
	Profile string `json:"profile"`
	// Namespaces limits which CloudWatch namespaces are collected.
	// Empty means all built-in defaults (EC2, RDS, ElastiCache, AmazonMQ,
	// ES, S3, ApplicationELB, NetworkELB).
	Namespaces []string `json:"namespaces"`
	// PollIntervalSeconds between GetMetricData calls.
	PollIntervalSeconds int `json:"poll_interval_seconds"`
	// DiscoveryIntervalMinutes between ListMetrics resource discovery runs.
	DiscoveryIntervalMinutes int `json:"discovery_interval_minutes"`
	// PeriodSeconds is the CloudWatch aggregation period requested.
	PeriodSeconds int `json:"period_seconds"`
}

// ---- native pollers (server mode): free in-VPC endpoints, no CloudWatch ----

type ValkeyTarget struct {
	Name                  string `json:"name"`
	Addr                  string `json:"addr"` // host:port
	Password              string `json:"password"`
	TLS                   bool   `json:"tls"` // ElastiCache in-transit encryption
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
}

type OpenSearchTarget struct {
	Name     string `json:"name"`
	URL      string `json:"url"` // https://vpc-xxx.region.es.amazonaws.com
	Username string `json:"username"`
	Password string `json:"password"`
	Insecure bool   `json:"insecure_skip_tls_verify"`
}

type RabbitTarget struct {
	Name     string `json:"name"`
	URL      string `json:"url"` // https://b-xxx.mq.region.amazonaws.com (mgmt API port 443 on AmazonMQ)
	Username string `json:"username"`
	Password string `json:"password"`
	Insecure bool   `json:"insecure_skip_tls_verify"`
}

type NativeConfig struct {
	PollIntervalSeconds int                `json:"poll_interval_seconds"`
	Valkey              []ValkeyTarget     `json:"valkey"`
	OpenSearch          []OpenSearchTarget `json:"opensearch"`
	RabbitMQ            []RabbitTarget     `json:"rabbitmq"`
}

// ---- agent mode ----

type AgentConfig struct {
	ServerURL       string   `json:"server_url"`
	Token           string   `json:"token"`
	IntervalSeconds int      `json:"interval_seconds"`
	LogGlobs        []string `json:"log_globs"`
	// KubeLogs tails /var/log/containers/*.log (DaemonSet on EKS nodes).
	KubeLogs bool   `json:"kube_logs"`
	Hostname string `json:"hostname"` // defaults to os.Hostname()
}

type Config struct {
	Listen       string       `json:"listen"`
	RetentionCap int          `json:"retention_points"` // ring buffer points per series
	AWS          AWSConfig    `json:"aws"`
	Kubernetes   K8sConfig    `json:"kubernetes"`
	Native       NativeConfig `json:"native"`
	Agent        AgentConfig  `json:"agent"`
	// IngestToken guards the push API; agents must present it as a bearer
	// token. Empty means unauthenticated ingest (fine on localhost only).
	IngestToken       string `json:"ingest_token"`
	LogRetentionLines int    `json:"log_retention_lines"`
}

func Default() Config {
	return Config{
		Listen:            ":8080",
		RetentionCap:      720,
		LogRetentionLines: 2000,
		AWS: AWSConfig{
			Enabled:                  true,
			PollIntervalSeconds:      300,
			DiscoveryIntervalMinutes: 10,
			PeriodSeconds:            60,
		},
		Kubernetes: K8sConfig{
			Enabled:             true,
			PollIntervalSeconds: 15,
		},
		Native: NativeConfig{PollIntervalSeconds: 15},
		Agent:  AgentConfig{IntervalSeconds: 15},
	}
}

// Load reads path (optional; "" uses pure defaults) then applies env overrides:
// AWSOBS_LISTEN, AWS_REGION, AWSOBS_K8S_API_URL, AWSOBS_K8S_TOKEN.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	if v := os.Getenv("AWSOBS_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("AWS_REGION"); v != "" && cfg.AWS.Region == "" {
		cfg.AWS.Region = v
	}
	if v := os.Getenv("AWS_PROFILE"); v != "" && cfg.AWS.Profile == "" {
		cfg.AWS.Profile = v
	}
	if v := os.Getenv("AWSOBS_K8S_API_URL"); v != "" {
		cfg.Kubernetes.APIURL = v
	}
	if v := os.Getenv("AWSOBS_K8S_TOKEN"); v != "" {
		cfg.Kubernetes.BearerToken = v
	}
	if v := os.Getenv("AWSOBS_SERVER_URL"); v != "" {
		cfg.Agent.ServerURL = v
	}
	if v := os.Getenv("AWSOBS_TOKEN"); v != "" {
		cfg.Agent.Token = v
		if cfg.IngestToken == "" {
			cfg.IngestToken = v
		}
	}
	if v := os.Getenv("AWSOBS_INGEST_TOKEN"); v != "" {
		cfg.IngestToken = v
	}
	if v := os.Getenv("AWSOBS_AGENT_KUBELOGS"); v == "true" || v == "1" {
		cfg.Agent.KubeLogs = true
	}
	if v := os.Getenv("AWSOBS_HOSTNAME"); v != "" {
		cfg.Agent.Hostname = v
	}
	if cfg.AWS.PollIntervalSeconds < 30 {
		cfg.AWS.PollIntervalSeconds = 30 // protect against API cost surprises
	}
	if cfg.Kubernetes.PollIntervalSeconds < 5 {
		cfg.Kubernetes.PollIntervalSeconds = 5
	}
	return cfg, nil
}

func (c AWSConfig) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}
func (c AWSConfig) DiscoveryInterval() time.Duration {
	return time.Duration(c.DiscoveryIntervalMinutes) * time.Minute
}
func (c K8sConfig) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}
