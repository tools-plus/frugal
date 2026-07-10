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
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Node       string   `json:"node"`
	Restarts   int      `json:"restarts"`
	Containers []string `json:"containers"`
	StartTime  string   `json:"startTime"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
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
		info := PodInfo{
			Namespace: p.Metadata.Namespace,
			Name:      p.Metadata.Name,
			Phase:     p.Status.Phase,
			Node:      p.Spec.NodeName,
			StartTime: p.Status.StartTime,
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
