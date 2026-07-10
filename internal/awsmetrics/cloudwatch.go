// Package awsmetrics collects metrics for AWS managed services via
// CloudWatch. Discovery uses ListMetrics (one AWS API dependency, no
// per-service Describe* clients) and collection uses GetMetricData batched
// up to 500 queries per call. It also serves on-demand history queries so
// the dashboard can show ranges longer than the in-memory ring buffer.
package awsmetrics

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/example/awsobs/internal/config"
	"github.com/example/awsobs/internal/store"
)

type metricDef struct {
	Name string
	Stat string
}

// defaults maps CloudWatch namespaces to curated metric sets. Namespaces
// listed in wildcardNamespaces (or configured but absent here) fall back to
// discovering every metric in the namespace.
var defaults = map[string][]metricDef{
	"AWS/EC2": {
		{"CPUUtilization", "Average"},
		{"NetworkIn", "Sum"},
		{"NetworkOut", "Sum"},
		{"EBSReadBytes", "Sum"},
		{"EBSWriteBytes", "Sum"},
		{"StatusCheckFailed", "Maximum"},
	},
	"AWS/RDS": {
		{"CPUUtilization", "Average"},
		{"FreeableMemory", "Average"},
		{"DatabaseConnections", "Average"},
		{"ReadLatency", "Average"},
		{"WriteLatency", "Average"},
		{"FreeStorageSpace", "Average"},
		{"ReadIOPS", "Average"},
		{"WriteIOPS", "Average"},
	},
	"AWS/DocDB": {
		{"CPUUtilization", "Average"},
		{"FreeableMemory", "Average"},
		{"DatabaseConnections", "Average"},
		{"ReadLatency", "Average"},
		{"WriteLatency", "Average"},
		{"VolumeBytesUsed", "Average"},
		{"BufferCacheHitRatio", "Average"},
	},
	// ElastiCache covers Valkey/Redis-compatible caches.
	"AWS/ElastiCache": {
		{"CPUUtilization", "Average"},
		{"EngineCPUUtilization", "Average"},
		{"DatabaseMemoryUsagePercentage", "Average"},
		{"BytesUsedForCache", "Average"},
		{"CurrConnections", "Average"},
		{"CacheHits", "Sum"},
		{"CacheMisses", "Sum"},
		{"Evictions", "Sum"},
	},
	// AmazonMQ metric names differ by engine; both sets listed, ListMetrics
	// simply returns nothing for the engine you don't run.
	"AWS/AmazonMQ": {
		// ActiveMQ
		{"CpuUtilization", "Average"},
		{"HeapUsage", "Average"},
		{"TotalMessageCount", "Average"},
		{"TotalConsumerCount", "Average"},
		{"TotalProducerCount", "Average"},
		// RabbitMQ
		{"SystemCpuUtilization", "Average"},
		{"RabbitMQMemUsed", "Average"},
		{"RabbitMQMemLimit", "Average"},
		{"RabbitMQDiskFree", "Average"},
		{"MessageCount", "Average"},
		{"ConsumerCount", "Average"},
		{"PublishRate", "Average"},
		{"AckRate", "Average"},
	},
	// OpenSearch still reports under the legacy Elasticsearch namespace.
	"AWS/ES": {
		{"CPUUtilization", "Average"},
		{"JVMMemoryPressure", "Average"},
		{"FreeStorageSpace", "Minimum"},
		{"ClusterStatus.red", "Maximum"},
		{"ClusterStatus.yellow", "Maximum"},
		{"SearchLatency", "Average"},
		{"IndexingLatency", "Average"},
	},
	// S3 storage metrics are emitted once per day by AWS.
	"AWS/S3": {
		{"BucketSizeBytes", "Average"},
		{"NumberOfObjects", "Average"},
	},
	"AWS/ApplicationELB": {
		{"RequestCount", "Sum"},
		{"TargetResponseTime", "Average"},
		{"HTTPCode_Target_5XX_Count", "Sum"},
		{"HTTPCode_Target_4XX_Count", "Sum"},
		{"HealthyHostCount", "Minimum"},
		{"UnHealthyHostCount", "Maximum"},
		{"ActiveConnectionCount", "Sum"},
	},
	"AWS/NetworkELB": {
		{"ActiveFlowCount", "Average"},
		{"NewFlowCount", "Sum"},
		{"ProcessedBytes", "Sum"},
		{"HealthyHostCount", "Minimum"},
		{"UnHealthyHostCount", "Maximum"},
	},
	// Container Insights (only populated if you've enabled the CloudWatch
	// agent / Container Insights on the cluster).
	"ContainerInsights": {
		{"pod_cpu_utilization", "Average"},
		{"pod_memory_utilization", "Average"},
		{"node_cpu_utilization", "Average"},
		{"node_memory_utilization", "Average"},
		{"cluster_failed_node_count", "Average"},
	},
}

// wildcardNamespaces get full-namespace discovery: every metric name AWS
// publishes there becomes a target. Used where the metric set is small or
// changes often (EKS control-plane metrics are new and still growing).
var wildcardNamespaces = map[string]bool{
	"AWS/EKS": true,
}

const wildcardCap = 1500 // per-namespace safety cap on discovered targets

// resourceDims is the priority order for picking the dimension that names
// "the resource" a series belongs to. Everything else becomes the variant.
var resourceDims = []string{
	"ClusterName", "InstanceId",
	"DBInstanceIdentifier", "DBClusterIdentifier",
	"CacheClusterId", "ReplicationGroupId",
	"Broker", "DomainName", "BucketName",
	"LoadBalancer", "QueueName", "TopicName", "FunctionName",
}

type target struct {
	Namespace string
	Metric    metricDef
	Dims      []types.Dimension
	SeriesID  string
	Labels    map[string]string
}

type Collector struct {
	cfg    config.AWSConfig
	cw     *cloudwatch.Client
	store  *store.Store
	logger *log.Logger

	mu      sync.RWMutex
	targets []target
	reg     map[string]target // series ID -> target, for History()
}

func New(ctx context.Context, cfg config.AWSConfig, st *store.Store, logger *log.Logger) (*Collector, error) {
	opts := []func(*awscfg.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awscfg.WithRegion(cfg.Region))
	}
	if cfg.Profile != "" {
		opts = append(opts, awscfg.WithSharedConfigProfile(cfg.Profile))
	}
	ac, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Collector{
		cfg:    cfg,
		cw:     cloudwatch.NewFromConfig(ac),
		store:  st,
		logger: logger,
		reg:    map[string]target{},
	}, nil
}

func (c *Collector) Run(ctx context.Context) {
	if err := c.discover(ctx); err != nil {
		c.logger.Printf("aws: initial discovery failed: %v", err)
	}
	discoverT := time.NewTicker(c.cfg.DiscoveryInterval())
	pollT := time.NewTicker(c.cfg.PollInterval())
	defer discoverT.Stop()
	defer pollT.Stop()

	c.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-discoverT.C:
			if err := c.discover(ctx); err != nil {
				c.logger.Printf("aws: discovery failed: %v", err)
			}
		case <-pollT.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) discover(ctx context.Context) error {
	namespaces := c.cfg.Namespaces
	if len(namespaces) == 0 {
		for ns := range defaults {
			namespaces = append(namespaces, ns)
		}
		for ns := range wildcardNamespaces {
			namespaces = append(namespaces, ns)
		}
		sort.Strings(namespaces)
	}

	var targets []target
	for _, ns := range namespaces {
		var (
			found []target
			err   error
		)
		if defs, ok := defaults[ns]; ok && !wildcardNamespaces[ns] {
			found, err = c.discoverCurated(ctx, ns, defs)
		} else {
			found, err = c.discoverAll(ctx, ns)
		}
		if err != nil {
			return err
		}
		targets = append(targets, found...)
	}

	reg := make(map[string]target, len(targets))
	for _, t := range targets {
		reg[t.SeriesID] = t
	}
	c.mu.Lock()
	c.targets = targets
	c.reg = reg
	c.mu.Unlock()
	c.logger.Printf("aws: discovery found %d metric targets", len(targets))
	return nil
}

func (c *Collector) discoverCurated(ctx context.Context, ns string, defs []metricDef) ([]target, error) {
	var out []target
	for _, def := range defs {
		p := cloudwatch.NewListMetricsPaginator(c.cw, &cloudwatch.ListMetricsInput{
			Namespace:  aws.String(ns),
			MetricName: aws.String(def.Name),
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("ListMetrics %s/%s: %w", ns, def.Name, err)
			}
			for _, m := range page.Metrics {
				out = append(out, newTarget(ns, def, m.Dimensions))
			}
		}
	}
	return out, nil
}

// discoverAll takes every metric in a namespace (capped) with stat Average.
func (c *Collector) discoverAll(ctx context.Context, ns string) ([]target, error) {
	var out []target
	p := cloudwatch.NewListMetricsPaginator(c.cw, &cloudwatch.ListMetricsInput{
		Namespace: aws.String(ns),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ListMetrics %s: %w", ns, err)
		}
		for _, m := range page.Metrics {
			out = append(out, newTarget(ns, metricDef{aws.ToString(m.MetricName), "Average"}, m.Dimensions))
			if len(out) >= wildcardCap {
				c.logger.Printf("aws: %s hit wildcard cap (%d), truncating", ns, wildcardCap)
				return out, nil
			}
		}
	}
	return out, nil
}

func newTarget(ns string, def metricDef, dims []types.Dimension) target {
	labels := map[string]string{
		"source":    "cloudwatch",
		"namespace": ns,
		"metric":    def.Name,
		"stat":      def.Stat,
	}
	sort.Slice(dims, func(i, j int) bool {
		return aws.ToString(dims[i].Name) < aws.ToString(dims[j].Name)
	})
	dimMap := map[string]string{}
	for _, d := range dims {
		name, val := aws.ToString(d.Name), aws.ToString(d.Value)
		labels[name] = val
		dimMap[name] = val
	}

	// Pick the resource dimension; everything else is the variant.
	resource := ""
	for _, rd := range resourceDims {
		if v, ok := dimMap[rd]; ok {
			resource = v
			delete(dimMap, rd)
			break
		}
	}
	if resource == "" {
		resource = "_aggregate"
	}
	variantParts := make([]string, 0, len(dimMap))
	for _, d := range dims {
		if v, ok := dimMap[aws.ToString(d.Name)]; ok {
			variantParts = append(variantParts, v)
		}
	}
	labels["resource"] = resource
	labels["variant"] = strings.Join(variantParts, " / ")

	parts := []string{"cw", strings.TrimPrefix(ns, "AWS/"), def.Name}
	for _, d := range dims {
		parts = append(parts, aws.ToString(d.Value))
	}
	return target{
		Namespace: ns,
		Metric:    def,
		Dims:      dims,
		SeriesID:  strings.Join(parts, "|"),
		Labels:    labels,
	}
}

func (c *Collector) poll(ctx context.Context) {
	c.mu.RLock()
	targets := c.targets
	c.mu.RUnlock()
	if len(targets) == 0 {
		return
	}
	period := int32(c.cfg.PeriodSeconds)
	if period < 60 {
		period = 60
	}
	end := time.Now()
	start := end.Add(-5 * time.Duration(period) * time.Second)

	const batchSize = 500
	for off := 0; off < len(targets); off += batchSize {
		hi := off + batchSize
		if hi > len(targets) {
			hi = len(targets)
		}
		batch := targets[off:hi]

		queries := make([]types.MetricDataQuery, len(batch))
		for i, t := range batch {
			queries[i] = mdq(fmt.Sprintf("q%d", i), t, period)
		}
		p := cloudwatch.NewGetMetricDataPaginator(c.cw, &cloudwatch.GetMetricDataInput{
			StartTime:         aws.Time(start),
			EndTime:           aws.Time(end),
			MetricDataQueries: queries,
			ScanBy:            types.ScanByTimestampAscending,
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				c.logger.Printf("aws: GetMetricData: %v", err)
				break
			}
			for _, r := range page.MetricDataResults {
				var idx int
				if _, err := fmt.Sscanf(aws.ToString(r.Id), "q%d", &idx); err != nil || idx >= len(batch) {
					continue
				}
				t := batch[idx]
				for i := range r.Timestamps {
					c.store.Add(t.SeriesID, t.Labels, store.Point{
						T: r.Timestamps[i].Unix(),
						V: r.Values[i],
					})
				}
			}
		}
	}
}

func mdq(id string, t target, period int32) types.MetricDataQuery {
	return types.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &types.MetricStat{
			Metric: &types.Metric{
				Namespace:  aws.String(t.Namespace),
				MetricName: aws.String(t.Metric.Name),
				Dimensions: t.Dims,
			},
			Period: aws.Int32(period),
			Stat:   aws.String(t.Metric.Stat),
		},
		ReturnData: aws.Bool(true),
	}
}

// History fetches a series straight from CloudWatch for arbitrary ranges —
// used by the dashboard for windows longer than the in-memory ring buffer.
// The period widens with the range to stay within CloudWatch retention tiers.
func (c *Collector) History(ctx context.Context, id string, from, to time.Time) ([]store.Point, error) {
	c.mu.RLock()
	t, ok := c.reg[id]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown series %q", id)
	}
	span := to.Sub(from)
	var period int32
	switch {
	case span <= 6*time.Hour:
		period = 60
	case span <= 48*time.Hour:
		period = 300
	case span <= 7*24*time.Hour:
		period = 900
	default:
		period = 3600
	}

	var out []store.Point
	p := cloudwatch.NewGetMetricDataPaginator(c.cw, &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(from),
		EndTime:           aws.Time(to),
		MetricDataQueries: []types.MetricDataQuery{mdq("h0", t, period)},
		ScanBy:            types.ScanByTimestampAscending,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.MetricDataResults {
			for i := range r.Timestamps {
				out = append(out, store.Point{T: r.Timestamps[i].Unix(), V: r.Values[i]})
			}
		}
	}
	return out, nil
}
