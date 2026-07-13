package k8s

import (
	"encoding/base64"
	"testing"
)

func TestParseKubeconfig(t *testing.T) {
	ca := base64.StdEncoding.EncodeToString([]byte("CA-PEM"))
	// One EKS exec-auth context and one static-token context.
	yml := `
apiVersion: v1
clusters:
- name: eks-cl
  cluster:
    server: https://ABC.eks.amazonaws.com
    certificate-authority-data: ` + ca + `
- name: tok-cl
  cluster:
    server: https://tok.example
    certificate-authority-data: ` + ca + `
users:
- name: eks-user
  user:
    exec:
      command: aws
      args: ["--region", "ap-south-1", "eks", "get-token", "--cluster-name", "prod-eks"]
- name: tok-user
  user:
    token: abc123
contexts:
- name: eks-ctx
  context: { cluster: eks-cl, user: eks-user }
- name: tok-ctx
  context: { cluster: tok-cl, user: tok-user }
current-context: eks-ctx
`
	got, err := ParseKubeconfig([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 clusters, got %d: %+v", len(got), got)
	}
	byName := map[string]KubeCluster{}
	for _, c := range got {
		byName[c.Name] = c
	}
	eks := byName["eks-ctx"]
	if eks.Server != "https://ABC.eks.amazonaws.com" || string(eks.CAData) != "CA-PEM" {
		t.Fatalf("eks server/ca: %+v", eks)
	}
	if eks.EKSClusterName != "prod-eks" || eks.Region != "ap-south-1" {
		t.Fatalf("eks exec parse: cluster=%q region=%q", eks.EKSClusterName, eks.Region)
	}
	if tok := byName["tok-ctx"]; tok.Token != "abc123" {
		t.Fatalf("token auth: %+v", tok)
	}
}
