// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// Package awsdiscovery uses the AWS Describe/List APIs (the same access keys the
// CloudWatch collector uses) to enumerate resources and their endpoints, so the
// free direct pollers can auto-attach without the operator hand-entering
// connection URLs. This is the "discover, then poll directly; CloudWatch only
// as fallback" path. Auth (AUTH tokens / passwords) still comes from config for
// secured resources; discovery supplies the endpoints.
package awsdiscovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/mq"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"

	"github.com/tools-plus/frugal/internal/config"
)

// EKSCluster is a discovered EKS cluster ready to connect to: its API endpoint,
// decoded CA cert, and the name/region used to mint an auth token.
type EKSCluster struct {
	Name     string
	Endpoint string
	CAData   []byte
	Region   string
}

// EKS discovers ACTIVE EKS clusters via the AWS API (eks:ListClusters +
// DescribeCluster) so frugal can collect node/pod metrics straight from creds —
// no kubeconfig and no kubectl. The caller mints a token per cluster (see
// internal/ekstoken) and connects directly. Clusters whose control-plane
// endpoint isn't reachable from the server simply fail to collect (logged);
// discovery still lists them. Per-cluster describe failures are skipped rather
// than aborting the whole scan.
func EKS(ctx context.Context, ac aws.Config) ([]EKSCluster, error) {
	c := eks.NewFromConfig(ac)
	var names []string
	p := eks.NewListClustersPaginator(c, &eks.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ListClusters: %w", err)
		}
		names = append(names, page.Clusters...)
	}
	var out []EKSCluster
	for _, name := range names {
		d, err := c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
		if err != nil || d.Cluster == nil {
			continue // skip clusters we can't describe rather than failing the scan
		}
		cl := d.Cluster
		if cl.Status != ekstypes.ClusterStatusActive || aws.ToString(cl.Endpoint) == "" {
			continue
		}
		var ca []byte
		if cl.CertificateAuthority != nil {
			ca, _ = base64.StdEncoding.DecodeString(aws.ToString(cl.CertificateAuthority.Data))
		}
		out = append(out, EKSCluster{
			Name:     name,
			Endpoint: aws.ToString(cl.Endpoint),
			CAData:   ca,
			Region:   ac.Region,
		})
	}
	return out, nil
}

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

// OpenSearch discovers OpenSearch/Elasticsearch domain endpoints. Most domains
// use fine-grained access control, so username/password comes from config
// (matched by name); discovery supplies the URL.
func OpenSearch(ctx context.Context, ac aws.Config) ([]config.OpenSearchTarget, error) {
	os := opensearch.NewFromConfig(ac)
	names, err := os.ListDomainNames(ctx, &opensearch.ListDomainNamesInput{})
	if err != nil {
		return nil, fmt.Errorf("ListDomainNames: %w", err)
	}
	var domainNames []string
	for _, d := range names.DomainNames {
		domainNames = append(domainNames, aws.ToString(d.DomainName))
	}
	if len(domainNames) == 0 {
		return nil, nil
	}
	desc, err := os.DescribeDomains(ctx, &opensearch.DescribeDomainsInput{DomainNames: domainNames})
	if err != nil {
		return nil, fmt.Errorf("DescribeDomains: %w", err)
	}
	var out []config.OpenSearchTarget
	for _, d := range desc.DomainStatusList {
		host := aws.ToString(d.Endpoint) // public endpoint
		if host == "" {
			host = d.Endpoints["vpc"] // VPC domains expose the vpc endpoint
		}
		if host == "" {
			continue
		}
		out = append(out, config.OpenSearchTarget{
			Name: aws.ToString(d.DomainName),
			URL:  "https://" + host,
		})
	}
	return out, nil
}

// RabbitMQ discovers AmazonMQ RabbitMQ broker management endpoints (ActiveMQ is
// skipped — it doesn't expose the RabbitMQ management HTTP API). Broker admin
// credentials come from config (matched by name).
func RabbitMQ(ctx context.Context, ac aws.Config) ([]config.RabbitTarget, error) {
	c := mq.NewFromConfig(ac)
	var out []config.RabbitTarget
	p := mq.NewListBrokersPaginator(c, &mq.ListBrokersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ListBrokers: %w", err)
		}
		for _, b := range page.BrokerSummaries {
			if string(b.EngineType) != "RABBITMQ" {
				continue
			}
			d, err := c.DescribeBroker(ctx, &mq.DescribeBrokerInput{BrokerId: b.BrokerId})
			if err != nil {
				return nil, fmt.Errorf("DescribeBroker: %w", err)
			}
			url := ""
			if len(d.BrokerInstances) > 0 {
				url = aws.ToString(d.BrokerInstances[0].ConsoleURL) // RabbitMQ mgmt API + console share this host
			}
			if url == "" {
				continue
			}
			out = append(out, config.RabbitTarget{
				Name: aws.ToString(d.BrokerName),
				URL:  url,
			})
		}
	}
	return out, nil
}
