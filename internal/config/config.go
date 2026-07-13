// Package config loads awsobs configuration from a JSON file with
// environment-variable overrides. JSON keeps the binary dependency-free.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ClusterConfig identifies one Kubernetes cluster to collect from.
type ClusterConfig struct {
	Name string `json:"name"`
	// APIURL, e.g. http://127.0.0.1:8001 for `kubectl proxy --port 8001`.
	// Empty means autodetect (in-cluster ServiceAccount, else proxy on 8001).
	APIURL                string `json:"api_url"`
	BearerToken           string `json:"bearer_token"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
}

type K8sConfig struct {
	Enabled bool `json:"enabled"`
	// Namespaces to watch; empty means all namespaces.
	Namespaces []string `json:"namespaces"`
	// PollIntervalSeconds for pod/node metrics (metrics-server resolution is ~15s).
	PollIntervalSeconds int `json:"poll_interval_seconds"`
	// Contexts are kubeconfig context names (`kubectl config get-contexts`).
	// awsobs spawns and supervises a kubectl proxy per context — the easy
	// path for local use. "*" expands to every context in the kubeconfig.
	Contexts []string `json:"contexts"`
	// Clusters are direct API endpoints (your own proxy, or api_url +
	// bearer_token). Can be combined with contexts.
	Clusters []ClusterConfig `json:"clusters"`
	// Kubeconfig is an uploaded kubeconfig (YAML). Each context becomes a
	// collected cluster; EKS exec-auth contexts are authenticated with tokens
	// awsobs mints from the configured AWS credentials. Encrypted at rest.
	Kubeconfig string `json:"kubeconfig"`

	// Legacy single-cluster fields (used only when clusters is empty).
	Name                  string `json:"cluster_name"`
	APIURL                string `json:"api_url"`
	BearerToken           string `json:"bearer_token"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
}

// ClusterList normalizes config into a list of clusters.
func (c K8sConfig) ClusterList() []ClusterConfig {
	if len(c.Clusters) > 0 {
		return c.Clusters
	}
	name := c.Name
	if name == "" {
		name = "default"
	}
	return []ClusterConfig{{
		Name: name, APIURL: c.APIURL,
		BearerToken: c.BearerToken, InsecureSkipTLSVerify: c.InsecureSkipTLSVerify,
	}}
}

type AWSConfig struct {
	Enabled bool   `json:"enabled"`
	Region  string `json:"region"`
	Profile string `json:"profile"`
	// Static credentials (optional). When empty, the default AWS credential
	// chain is used (env vars, shared config, SSO, EC2/EKS instance role/IRSA).
	// SecretAccessKey and SessionToken are encrypted at rest.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
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

// AuthConfig gates the dashboard behind a login. Enabled defaults to true; set
// "auth": {"enabled": false} to serve without authentication (e.g. behind a
// trusted VPN / port-forward, or for local dev).
type AuthConfig struct {
	// Enabled is a pointer so an omitted or empty "auth" block still defaults
	// to on — only an explicit `false` disables auth.
	Enabled *bool `json:"enabled"`
	// DBPath is the auth SQLite file. Empty means <data_dir>/auth.db, or
	// ./awsobs-auth.db when data_dir is unset.
	DBPath string `json:"db_path"`
}

// On reports whether authentication is enabled (the default).
func (a AuthConfig) On() bool { return a.Enabled == nil || *a.Enabled }

type Config struct {
	Listen       string       `json:"listen"`
	RetentionCap int          `json:"retention_points"` // ring buffer points per series
	AWS          AWSConfig    `json:"aws"`
	Kubernetes   K8sConfig    `json:"kubernetes"`
	Native       NativeConfig `json:"native"`
	Agent        AgentConfig  `json:"agent"`
	Auth         AuthConfig   `json:"auth"`
	// IngestToken guards the push API; agents must present it as a bearer
	// token. Empty means unauthenticated ingest (fine on localhost only).
	IngestToken       string `json:"ingest_token"`
	LogRetentionLines int    `json:"log_retention_lines"`
	// DataDir enables SQLite persistence: <data_dir>/awsobs.db stores all
	// polled series, points, logs, and pod inventory, hydrating the
	// dashboard instantly on restart. Empty disables persistence.
	DataDir string `json:"data_dir"`
	// DBRetentionHours bounds how much point history SQLite keeps (default 72).
	DBRetentionHours int `json:"db_retention_hours"`
	// SecretKey encrypts credentials stored in the control DB (AWS keys, native
	// passwords, ingest token). Keep it out of source control; the env var
	// AWSOBS_SECRET_KEY overrides this and is preferable in production.
	SecretKey string `json:"secret_key"`
}

// Runtime is the operational config that lives in the control DB and is
// editable at runtime from the admin UI (as opposed to the bootstrap fields —
// listen, data_dir, auth — which stay in server.json / env). Secret fields
// within it are encrypted at rest.
type Runtime struct {
	AWS               AWSConfig    `json:"aws"`
	Kubernetes        K8sConfig    `json:"kubernetes"`
	Native            NativeConfig `json:"native"`
	IngestToken       string       `json:"ingest_token"`
	RetentionCap      int          `json:"retention_points"`
	LogRetentionLines int          `json:"log_retention_lines"`
	DBRetentionHours  int          `json:"db_retention_hours"`
}

// ToRuntime extracts the runtime subset of a Config — used to seed the control
// DB from server.json on first boot (migration).
func (c Config) ToRuntime() Runtime {
	return Runtime{
		AWS:               c.AWS,
		Kubernetes:        c.Kubernetes,
		Native:            c.Native,
		IngestToken:       c.IngestToken,
		RetentionCap:      c.RetentionCap,
		LogRetentionLines: c.LogRetentionLines,
		DBRetentionHours:  c.DBRetentionHours,
	}
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
	// Bootstrap keys: each server.json field can also come from the environment,
	// which wins over the file.
	if v := os.Getenv("AWSOBS_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("AWSOBS_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("AWSOBS_SECRET_KEY"); v != "" {
		cfg.SecretKey = v
	}
	if v := os.Getenv("AWSOBS_AUTH_ENABLED"); v != "" {
		b := v == "true" || v == "1"
		cfg.Auth.Enabled = &b
	}
	if v := os.Getenv("AWSOBS_AUTH_DB_PATH"); v != "" {
		cfg.Auth.DBPath = v
	}
	// Env overrides file — an explicitly exported AWS_REGION/AWS_PROFILE
	// should always win over a copied example config.
	if v := os.Getenv("AWS_REGION"); v != "" {
		cfg.AWS.Region = v
	}
	if v := os.Getenv("AWS_PROFILE"); v != "" {
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

// AuthDBPath resolves where the auth SQLite file lives: an explicit db_path,
// else alongside the metrics db in data_dir, else the working directory.
func (c Config) AuthDBPath() string {
	if c.Auth.DBPath != "" {
		return c.Auth.DBPath
	}
	if c.DataDir != "" {
		return filepath.Join(c.DataDir, "auth.db")
	}
	return "awsobs-auth.db"
}
