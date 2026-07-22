// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

package k8s

import "sync"

// Inventory is the shared pod-inventory cache. Collectors refresh it every
// poll (they list pods anyway for workload labels), so /api/pods serves
// from memory instead of hitting every cluster API on each page load.
type Inventory struct {
	mu        sync.RWMutex
	byCluster map[string][]PodInfo
}

func NewInventory() *Inventory {
	return &Inventory{byCluster: map[string][]PodInfo{}}
}

func (i *Inventory) Set(cluster string, pods []PodInfo) {
	cp := make([]PodInfo, len(pods))
	copy(cp, pods)
	for j := range cp {
		cp[j].Cluster = cluster
	}
	i.mu.Lock()
	i.byCluster[cluster] = cp
	i.mu.Unlock()
}

func (i *Inventory) All() []PodInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := []PodInfo{}
	for _, pods := range i.byCluster {
		out = append(out, pods...)
	}
	return out
}
