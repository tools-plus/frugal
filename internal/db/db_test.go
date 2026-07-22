//go:build cgo

package db

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/tools-plus/awsobs/internal/k8s"
	"github.com/tools-plus/awsobs/internal/logstore"
	"github.com/tools-plus/awsobs/internal/store"
)

func TestPersistAndHydrate(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(os.Stderr, "", 0)
	d, err := Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}

	st := store.New(10)
	ls := logstore.New(100)
	inv := k8s.NewInventory()
	ctx, cancel := context.WithCancel(context.Background())
	done := d.StartPersist(ctx, st, ls, inv)

	now := time.Now().Unix()
	st.Add("s1", map[string]string{"metric": "cpu"}, store.Point{T: now - 10, V: 1})
	st.Add("s1", map[string]string{"metric": "cpu"}, store.Point{T: now, V: 2})
	ls.Append("host/h1", []string{"line a", "line b"})
	inv.Set("c1", []k8s.PodInfo{{Namespace: "ns", Name: "p1", Phase: "Running", Workload: "w", WorkloadKind: "Deployment"}})
	time.Sleep(2500 * time.Millisecond) // one flush tick
	d.savePods(inv)
	cancel()
	<-done
	d.Close()

	// fresh process simulation: reopen and hydrate empty stores
	d2, err := Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	st2 := store.New(10)
	ls2 := logstore.New(100)
	inv2 := k8s.NewInventory()
	d2.Hydrate(st2, ls2, inv2)

	pts := st2.Data("s1")
	if len(pts) != 2 || pts[1].V != 2 {
		t.Fatalf("points not hydrated: %+v", pts)
	}
	metas := st2.List("s1")
	if len(metas) != 1 || metas[0].Labels["metric"] != "cpu" {
		t.Fatalf("labels not hydrated: %+v", metas)
	}
	if lines := ls2.Tail("host/h1", 10); len(lines) != 2 || lines[1] != "line b" {
		t.Fatalf("logs not hydrated: %v", lines)
	}
	pods := inv2.All()
	if len(pods) != 1 || pods[0].Cluster != "c1" || pods[0].Workload != "w" {
		t.Fatalf("pods not hydrated: %+v", pods)
	}
}
