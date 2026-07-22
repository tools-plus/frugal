// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package k8s

import (
	"encoding/base64"
	"fmt"

	"gopkg.in/yaml.v3"
)

// KubeCluster is one connectable cluster resolved from a kubeconfig context:
// the API endpoint + CA, plus exactly one auth method. For EKS exec-auth
// entries only the cluster name + region are captured — frugal mints the token
// itself (see internal/ekstoken) rather than running the exec plugin.
type KubeCluster struct {
	Name   string // context name (display)
	Server string
	CAData []byte

	// auth — one of:
	Token          string // static bearer token
	ClientCertData []byte // client-cert mTLS
	ClientKeyData  []byte
	EKSClusterName string // exec-based EKS → mint a token for this cluster
	Region         string // region for the EKS token (from exec args/env)
}

// kubeconfig is the subset of the kubeconfig schema we read.
type kubeconfig struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
			Exec                  *struct {
				Command string   `yaml:"command"`
				Args    []string `yaml:"args"`
				Env     []struct {
					Name  string `yaml:"name"`
					Value string `yaml:"value"`
				} `yaml:"env"`
			} `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// ParseKubeconfig resolves every context into a connectable KubeCluster.
func ParseKubeconfig(data []byte) ([]KubeCluster, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	clusters := map[string]struct {
		server string
		ca     []byte
	}{}
	for _, c := range kc.Clusters {
		ca, _ := base64.StdEncoding.DecodeString(c.Cluster.CertificateAuthorityData)
		clusters[c.Name] = struct {
			server string
			ca     []byte
		}{c.Cluster.Server, ca}
	}
	type userAuth struct {
		token          string
		cert, key      []byte
		eksCluster     string
		region         string
	}
	users := map[string]userAuth{}
	for _, u := range kc.Users {
		var a userAuth
		a.token = u.User.Token
		a.cert, _ = base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
		a.key, _ = base64.StdEncoding.DecodeString(u.User.ClientKeyData)
		if u.User.Exec != nil { // EKS exec (aws eks get-token / aws-iam-authenticator token)
			a.eksCluster = argValue(u.User.Exec.Args, "--cluster-name", "-i", "cluster-id")
			a.region = argValue(u.User.Exec.Args, "--region")
			for _, e := range u.User.Exec.Env {
				if e.Name == "AWS_REGION" || e.Name == "AWS_DEFAULT_REGION" {
					a.region = e.Value
				}
			}
		}
		users[u.Name] = a
	}
	var out []KubeCluster
	for _, ctx := range kc.Contexts {
		cl, ok := clusters[ctx.Context.Cluster]
		if !ok || cl.server == "" {
			continue
		}
		a := users[ctx.Context.User]
		out = append(out, KubeCluster{
			Name:           ctx.Name,
			Server:         cl.server,
			CAData:         cl.ca,
			Token:          a.token,
			ClientCertData: a.cert,
			ClientKeyData:  a.key,
			EKSClusterName: a.eksCluster,
			Region:         a.region,
		})
	}
	return out, nil
}

// argValue returns the value following any of the given flags in args
// (supports "--flag value"; the "-i <name>" and positional "cluster-id" forms
// used by aws-iam-authenticator are also matched).
func argValue(args []string, flags ...string) string {
	want := map[string]bool{}
	for _, f := range flags {
		want[f] = true
	}
	for i := 0; i < len(args)-1; i++ {
		if want[args[i]] {
			return args[i+1]
		}
	}
	return ""
}
