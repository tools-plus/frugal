package k8s

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
)

// PodInfo is the pod inventory row shown in the dashboard.
type PodInfo struct {
	Cluster      string   `json:"cluster"` // filled in by the server
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Phase        string   `json:"phase"`
	Node         string   `json:"node"`
	Restarts     int      `json:"restarts"`
	Containers   []string `json:"containers"`
	StartTime    string   `json:"startTime"`
	Workload     string   `json:"workload"`
	WorkloadKind string   `json:"workloadKind"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			OwnerReferences []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			StartTime         string `json:"startTime"`
			ContainerStatuses []struct {
				RestartCount int `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// ListPods returns pod inventory across all namespaces (or the given one).
func (c *Client) ListPods(ctx context.Context, namespace string) ([]PodInfo, error) {
	path := "/api/v1/pods"
	if namespace != "" {
		path = "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
	}
	b, err := c.GetJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	var list podList
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	out := make([]PodInfo, 0, len(list.Items))
	for _, p := range list.Items {
		wl, kind := workloadOf(p.Metadata.OwnerReferences, p.Metadata.Name)
		info := PodInfo{
			Namespace:    p.Metadata.Namespace,
			Name:         p.Metadata.Name,
			Phase:        p.Status.Phase,
			Node:         p.Spec.NodeName,
			StartTime:    p.Status.StartTime,
			Workload:     wl,
			WorkloadKind: kind,
		}
		for _, cs := range p.Status.ContainerStatuses {
			info.Restarts += cs.RestartCount
		}
		for _, ct := range p.Spec.Containers {
			info.Containers = append(info.Containers, ct.Name)
		}
		out = append(out, info)
	}
	return out, nil
}

// StreamLogs opens a follow=true log stream for a pod — the same wire call
// `kubectl logs -f` makes. Caller must Close the reader.
func (c *Client) StreamLogs(ctx context.Context, namespace, pod, container string, tailLines int) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("follow", "true")
	q.Set("timestamps", "true")
	if tailLines > 0 {
		q.Set("tailLines", strconv.Itoa(tailLines))
	}
	if container != "" {
		q.Set("container", container)
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) +
		"/pods/" + url.PathEscape(pod) + "/log?" + q.Encode()
	return c.Stream(ctx, path)
}

type ownerRef struct{ Kind, Name string }

// workloadOf resolves a pod to its owning workload. ReplicaSet-owned pods
// roll up to their Deployment (strip the ReplicaSet hash suffix); other
// controllers use their own name; bare pods stand alone.
func workloadOf(owners []struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}, podName string) (string, string) {
	for _, o := range owners {
		switch o.Kind {
		case "ReplicaSet":
			if i := lastDash(o.Name); i > 0 {
				return o.Name[:i], "Deployment"
			}
			return o.Name, "ReplicaSet"
		case "StatefulSet", "DaemonSet", "Job", "CronJob", "Rollout":
			return o.Name, o.Kind
		}
	}
	return podName, "Pod"
}

func lastDash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '-' {
			return i
		}
	}
	return -1
}
