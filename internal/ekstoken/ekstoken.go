// Package ekstoken mints EKS API bearer tokens from AWS credentials — the same
// token `aws eks get-token` produces, so frugal can authenticate to an EKS
// cluster's API without kubectl, aws-cli, or an exec plugin. The token is a
// presigned STS GetCallerIdentity URL carrying the target cluster name in a
// signed header; presigning is done locally (no network call), using the
// resolved IRSA/static credentials. Tokens are short-lived (~15 min) — refresh
// periodically.
package ekstoken

import (
	"context"
	"encoding/base64"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	tokenPrefix   = "k8s-aws-v1."
	clusterHeader = "x-k8s-aws-id"
)

// Token returns an EKS bearer token for clusterName.
func Token(ctx context.Context, ac aws.Config, clusterName string) (string, error) {
	presign := sts.NewPresignClient(sts.NewFromConfig(ac))
	req, err := presign.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		// Add the cluster id as a signed header so EKS accepts the token.
		o.ClientOptions = append(o.ClientOptions, func(opts *sts.Options) {
			opts.APIOptions = append(opts.APIOptions, smithyhttp.SetHeaderValue(clusterHeader, clusterName))
		})
	})
	if err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}
