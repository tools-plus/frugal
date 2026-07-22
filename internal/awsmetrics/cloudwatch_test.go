// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package awsmetrics

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/tools-plus/frugal/internal/config"
)

func dim(n, v string) types.Dimension {
	return types.Dimension{Name: aws.String(n), Value: aws.String(v)}
}

func TestNewTargetResourceVariant(t *testing.T) {
	cases := []struct {
		dims              []types.Dimension
		resource, variant string
	}{
		{[]types.Dimension{dim("CacheClusterId", "dev-valkey-001"), dim("CacheNodeId", "0001")}, "dev-valkey-001", "0001"},
		{[]types.Dimension{dim("LoadBalancer", "app/web/abc"), dim("TargetGroup", "targetgroup/api/def")}, "app/web/abc", "targetgroup/api/def"},
		{[]types.Dimension{dim("InstanceId", "i-0abc")}, "i-0abc", ""},
		{[]types.Dimension{dim("BucketName", "my-bucket"), dim("StorageType", "StandardStorage")}, "my-bucket", "StandardStorage"},
		{nil, "_aggregate", ""},
	}
	for _, c := range cases {
		tg := newTarget("AWS/Test", metricDef{"M", "Average"}, c.dims)
		if tg.Labels["resource"] != c.resource || tg.Labels["variant"] != c.variant {
			t.Errorf("dims %v: got resource=%q variant=%q, want %q/%q",
				c.dims, tg.Labels["resource"], tg.Labels["variant"], c.resource, c.variant)
		}
	}
}

func TestEstMonthlyUSD(t *testing.T) {
	// 200 regular targets @ 300s poll → 200 × (2,592,000/300) × $0.00001 = $17.28.
	c := &Collector{cfg: config.AWSConfig{PollIntervalSeconds: 300}}
	if got := c.estMonthlyUSD(200, 0); got != 17.28 {
		t.Errorf("regular-only: got %v, want 17.28", got)
	}
	// Doubling the poll interval halves the cost.
	c.cfg.PollIntervalSeconds = 600
	if got := c.estMonthlyUSD(200, 0); got != 8.64 {
		t.Errorf("10-min poll: got %v, want 8.64", got)
	}
	// Daily targets are billed on the hourly cadence, not the poll interval:
	// 10 × (2,592,000/3600) × $0.00001 = $0.072 → rounds to $0.07.
	c.cfg.PollIntervalSeconds = 300
	if got := c.estMonthlyUSD(0, 10); got != 0.07 {
		t.Errorf("daily-only: got %v, want 0.07", got)
	}
}
