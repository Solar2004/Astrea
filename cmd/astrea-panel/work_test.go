package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkLine(t *testing.T) {
	end := time.Now()
	task := workTask{Machine: "f3-2", Title: "Vídeos <MiniMax>", Status: "done", Progress: "5/5",
		Started: end.Add(-12 * time.Minute), Ended: &end, Actions: 34, Errors: 2}
	got := workLine(task, true)
	for _, want := range []string{"✅", "f3-2", "<b>Vídeos &lt;MiniMax&gt;</b>", "5/5", "12 min", "34 acciones", "(2 ✗)"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
}

func TestCurrentWork(t *testing.T) {
	s := machineStatus{Name: "f3-1", Online: true, Admin: json.RawMessage(`{"mode":"yolo","work":{"id":"x","title":"Tests","status":"running"}}`)}
	if w, ok := currentWork(s); !ok || w.ID != "x" || w.Machine != "f3-1" {
		t.Fatalf("%+v %v", w, ok)
	}
	s.Admin = json.RawMessage(`{"mode":"yolo"}`)
	if _, ok := currentWork(s); ok {
		t.Fatal("no work expected")
	}
}

func TestWorkCacheStale(t *testing.T) {
	c := loadWorkCache(t.TempDir() + "/w.json")
	c.putList("f3-1", []workTask{{ID: "a", Title: "Vídeos", Status: "running"}})
	c.patch("f3-1", workTask{ID: "a", Title: "Vídeos", Status: "cancelled"})
	r := loadWorkCache(c.path) // survives a panel restart
	got, ok := r.list("f3-1")
	if !ok || len(got) != 1 || !got[0].Stale || got[0].CachedAt == nil || got[0].Status != "cancelled" || got[0].Machine != "f3-1" {
		t.Fatalf("%+v %v", got, ok)
	}
	r.forget(map[string]bool{})
	if _, ok := r.list("f3-1"); ok {
		t.Fatal("removed machine should be forgotten")
	}
}
