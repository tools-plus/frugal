// Package k8s is a deliberately tiny Kubernetes REST client. We only need
// three read paths (metrics.k8s.io, pod lists, pod logs), so plain HTTP
// beats pulling in the whole of client-go.
package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/example/awsobs/internal/config"
)

const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient picks an auth mode:
//  1. explicit api_url from config (with optional bearer token),
//  2. in-cluster ServiceAccount (when running inside the EKS cluster),
//  3. kubectl proxy at http://127.0.0.1:8001 (the zero-setup local dev path:
//     run `kubectl proxy` in another terminal).
func NewClient(cfg config.ClusterConfig) (*Client, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{}}
	c := &Client{http: &http.Client{Transport: tr, Timeout: 0}}

	switch {
	case cfg.APIURL != "":
		c.base = cfg.APIURL
		c.token = cfg.BearerToken
		if cfg.InsecureSkipTLSVerify {
			tr.TLSClientConfig.InsecureSkipVerify = true
		}
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		c.base = "https://" + host + ":" + port
		tok, err := os.ReadFile(saTokenPath)
		if err != nil {
			return nil, fmt.Errorf("in-cluster token: %w", err)
		}
		c.token = string(tok)
		ca, err := os.ReadFile(saCAPath)
		if err != nil {
			return nil, fmt.Errorf("in-cluster CA: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tr.TLSClientConfig.RootCAs = pool
	default:
		c.base = "http://127.0.0.1:8001" // kubectl proxy
	}
	return c, nil
}

func (c *Client) do(ctx context.Context, path string, timeout time.Duration) (*http.Response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		// resp.Body close by caller ends the request; timer cleanup via context.
		go func() { <-ctx.Done(); cancel() }()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, string(b))
	}
	return resp, nil
}

// GetJSON fetches path and returns the raw body (caller unmarshals).
func (c *Client) GetJSON(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.do(ctx, path, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Stream opens a long-lived response body (used for ?follow=true log tails).
// The caller must Close it.
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, path, 0)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
