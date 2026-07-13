// Package awsdiscovery uses the AWS Describe/List APIs (the same access keys the
// CloudWatch collector uses) to enumerate resources and their endpoints, so the
// free direct pollers can auto-attach without the operator hand-entering
// connection URLs. This is the "discover, then poll directly; CloudWatch only
// as fallback" path. Auth (AUTH tokens / passwords) still comes from config for
// secured resources; discovery supplies the endpoints.
package awsdiscovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"

	"github.com/example/awsobs/internal/config"
)

// Valkey discovers ElastiCache (Redis/Valkey) node endpoints. Passwords are
// left blank — unsecured in-VPC clusters poll as-is; for AUTH-enabled clusters
// the operator supplies the token in Settings (matched by name).
func Valkey(ctx context.Context, ac aws.Config) ([]config.ValkeyTarget, error) {
	ec := elasticache.NewFromConfig(ac)
	var out []config.ValkeyTarget
	p := elasticache.NewDescribeCacheClustersPaginator(ec, &elasticache.DescribeCacheClustersInput{
		ShowCacheNodeInfo: aws.Bool(true),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("DescribeCacheClusters: %w", err)
		}
		for _, cc := range page.CacheClusters {
			engine := strings.ToLower(aws.ToString(cc.Engine))
			if engine != "redis" && engine != "valkey" { // memcached uses a different protocol
				continue
			}
			id := aws.ToString(cc.CacheClusterId)
			tls := cc.TransitEncryptionEnabled != nil && *cc.TransitEncryptionEnabled
			for _, n := range cc.CacheNodes {
				if n.Endpoint == nil || n.Endpoint.Address == nil {
					continue
				}
				name := id
				if len(cc.CacheNodes) > 1 {
					name = id + "/" + aws.ToString(n.CacheNodeId)
				}
				out = append(out, config.ValkeyTarget{
					Name: name,
					Addr: fmt.Sprintf("%s:%d", aws.ToString(n.Endpoint.Address), aws.ToInt32(n.Endpoint.Port)),
					TLS:  tls,
				})
			}
		}
	}
	return out, nil
}
