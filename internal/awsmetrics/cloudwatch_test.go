// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package awsmetrics

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
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
