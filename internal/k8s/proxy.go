// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package k8s

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// This file lets config reference kubeconfig contexts directly
// (kubernetes.contexts: ["plane-eks-dev", ...] or ["*"]). For each context
// frugal spawns and supervises its own `kubectl proxy` on a local ephemeral
// port — kubectl keeps handling auth (EKS exec plugins, token refresh,
// client certs), so any context that works for `kubectl get pods` works
// here without client-go.

// ExpandContexts resolves the configured context list. A "*" entry expands
// to every context in the kubeconfig (`kubectl config get-contexts -o name`).
func ExpandContexts(ctx context.Context, patterns []string) ([]string, error) {
	wildcard := false
	for _, p := range patterns {
		if p == "*" {
			wildcard = true
		}
	}
	if !wildcard {
		return patterns, nil
	}
	out, err := exec.CommandContext(ctx, "kubectl", "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl config get-contexts: %w (is kubectl on PATH?)", err)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// ContextDisplayName shortens ARN-style EKS context names
// (arn:aws:eks:region:acct:cluster/NAME -> NAME).
func ContextDisplayName(c string) string {
	if strings.HasPrefix(c, "arn:aws:eks:") {
		if i := strings.LastIndex(c, "/"); i > 0 && i < len(c)-1 {
			return c[i+1:]
		}
	}
	return c
}

// StartProxy launches `kubectl proxy` for one context on a fixed free port,
// waits until it answers, and supervises it (restart with backoff on exit,
// same port so client URLs stay valid). Returns the proxy base URL.
func StartProxy(ctx context.Context, kubectlCtx string, logger *log.Logger) (string, error) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return "", fmt.Errorf("kubectl not found on PATH (needed for kubernetes.contexts)")
	}
	port, err := freePort()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	start := func() (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "kubectl", "proxy",
			"--context", kubectlCtx, "--address=127.0.0.1",
			fmt.Sprintf("--port=%d", port))
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		go func() { // surface kubectl errors in our log
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				logger.Printf("kubectl-proxy(%s): %s", kubectlCtx, sc.Text())
			}
		}()
		return cmd, nil
	}

	cmd, err := start()
	if err != nil {
		return "", fmt.Errorf("kubectl proxy --context %s: %w", kubectlCtx, err)
	}
	if err := waitReady(ctx, url, 20*time.Second); err != nil {
		cmd.Process.Kill()
		return "", fmt.Errorf("proxy for context %s never became ready: %w", kubectlCtx, err)
	}

	go func() { // supervisor: restart on exit until shutdown
		c := cmd
		for {
			c.Wait()
			if ctx.Err() != nil {
				return
			}
			logger.Printf("kubectl-proxy(%s): exited, restarting in 3s", kubectlCtx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			nc, err := start()
			if err != nil {
				logger.Printf("kubectl-proxy(%s): restart failed: %v", kubectlCtx, err)
				continue
			}
			c = nc
		}
	}()
	return url, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	cl := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := cl.Get(url + "/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s", timeout)
}
