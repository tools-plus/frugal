// Package piwatch pulls RDS Performance Insights metrics (db load / active
// sessions) directly from the pi:GetResourceMetrics API — a free (7-day
// retention) source that complements the CloudWatch RDS metrics with DB-level
// load that CloudWatch doesn't expose. Only instances with Performance Insights
// enabled produce data. Series are labeled under the RDS service so they group
// with the CloudWatch metrics for the same instance.
package piwatch

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pi"
	pitypes "github.com/aws/aws-sdk-go-v2/service/pi/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/example/awsobs/internal/config"
	"github.com/example/awsobs/internal/store"
)

// piMetrics are the Performance Insights metrics we pull, with dashboard names.
var piMetrics = []struct{ query, label string }{
	{"db.load.avg", "db_load"}, // average active sessions (the flagship PI metric)
}

type Collector struct {
	rds    *rds.Client
	pi     *pi.Client
	store  *store.Store
	logger *log.Logger
	poll   time.Duration
}

func New(ac aws.Config, cfg config.AWSConfig, st *store.Store, logger *log.Logger) *Collector {
	poll := cfg.PollInterval()
	if poll < time.Minute {
		poll = time.Minute // PI granularity is 1 minute
	}
	return &Collector{rds: rds.NewFromConfig(ac), pi: pi.NewFromConfig(ac), store: st, logger: logger, poll: poll}
}

func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.poll)
	defer t.Stop()
	c.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pollOnce(ctx)
		}
	}
}

type dbInst struct{ id, resourceID string }

// instances lists RDS instances with Performance Insights enabled.
func (c *Collector) instances(ctx context.Context) ([]dbInst, error) {
	var out []dbInst
	p := rds.NewDescribeDBInstancesPaginator(c.rds, &rds.DescribeDBInstancesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range page.DBInstances {
			if d.PerformanceInsightsEnabled == nil || !*d.PerformanceInsightsEnabled || d.DbiResourceId == nil {
				continue
			}
			out = append(out, dbInst{id: aws.ToString(d.DBInstanceIdentifier), resourceID: aws.ToString(d.DbiResourceId)})
		}
	}
	return out, nil
}

func (c *Collector) pollOnce(ctx context.Context) {
	insts, err := c.instances(ctx)
	if err != nil {
		c.logger.Printf("pi: DescribeDBInstances: %v", err)
		return
	}
	queries := make([]pitypes.MetricQuery, len(piMetrics))
	for i, m := range piMetrics {
		queries[i] = pitypes.MetricQuery{Metric: aws.String(m.query)}
	}
	end := time.Now()
	start := end.Add(-5 * time.Minute)
	for _, inst := range insts {
		res, err := c.pi.GetResourceMetrics(ctx, &pi.GetResourceMetricsInput{
			ServiceType:     pitypes.ServiceTypeRds,
			Identifier:      aws.String(inst.resourceID),
			MetricQueries:   queries,
			StartTime:       aws.Time(start),
			EndTime:         aws.Time(end),
			PeriodInSeconds: aws.Int32(60),
		})
		if err != nil {
			c.logger.Printf("pi(%s): GetResourceMetrics: %v", inst.id, err)
			continue
		}
		for _, ml := range res.MetricList {
			if ml.Key == nil {
				continue
			}
			label := metricLabel(aws.ToString(ml.Key.Metric))
			id := strings.Join([]string{"pi", "RDS", label, inst.id}, "|")
			labels := map[string]string{"source": "pi", "namespace": "AWS/RDS", "resource": inst.id, "metric": label}
			for _, dp := range ml.DataPoints {
				if dp.Value == nil || dp.Timestamp == nil {
					continue
				}
				c.store.Add(id, labels, store.Point{T: dp.Timestamp.Unix(), V: *dp.Value})
			}
		}
	}
}

func metricLabel(q string) string {
	for _, m := range piMetrics {
		if m.query == q {
			return m.label
		}
	}
	return strings.ReplaceAll(q, ".", "_")
}
