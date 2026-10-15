package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleMemory: la memoria de un equipo (memory.go del agente).
//
//	GET    /api/memory?machine=[&refresh=1]  → {summary, inventory, notes}
//	POST   /api/memory {machine, text, tags} → nota nueva (marcada "panel")
//	DELETE /api/memory {machine, id}         → borra una nota
func (p *panel) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var out json.RawMessage
		args := map[string]any{"refresh": r.URL.Query().Get("refresh") == "1"}
		if code, err := p.call(r.Context(), r.URL.Query().Get("machine"), "memory.get", args, &out, 120*time.Second); err != nil {
			fail(w, code, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}
	if !mutating(w, r, http.MethodPost, http.MethodDelete) {
		return
	}
	var req struct {
		Machine string   `json:"machine"`
		Text    string   `json:"text"`
		Tags    []string `json:"tags"`
		ID      string   `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	method, args := "memory.note", map[string]any{"text": req.Text, "tags": req.Tags, "from": "panel"}
	if r.Method == http.MethodDelete {
		method, args = "memory.forget", map[string]any{"id": req.ID}
	}
	var out json.RawMessage
	if code, err := p.call(r.Context(), req.Machine, method, args, &out, 15*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
