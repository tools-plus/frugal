// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package ekstoken

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// TestToken verifies the token format offline (presigning is local, no network).
func TestToken(t *testing.T) {
	ac := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secretkeyexample", ""),
	}
	tok, err := Token(context.Background(), ac, "my-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "k8s-aws-v1.") {
		t.Fatalf("missing prefix: %s", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, "k8s-aws-v1."))
	if err != nil {
		t.Fatalf("token not base64url: %v", err)
	}
	url := string(raw)
	for _, want := range []string{"Action=GetCallerIdentity", "X-Amz-SignedHeaders=", "x-k8s-aws-id", "X-Amz-Signature="} {
		if !strings.Contains(url, want) {
			t.Fatalf("presigned url missing %q:\n%s", want, url)
		}
	}
}
