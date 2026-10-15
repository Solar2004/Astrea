// Despertar a Hermes: los avisos importantes (events.go) se mandan a la ruta
// de webhook de Hermes, que arranca una ejecución de la IA con el texto del
// aviso. Se activa con hermes.json junto al archivo de máquinas (se relee en
// cada envío, así que no hace falta reiniciar el panel):
//
//	{"url": "http://127.0.0.1:8644/webhooks/astrea", "secret": "…",
//	 "kinds": ["work.failed", "job.done", …]}   // kinds opcional
//
// Firma "generic V2" de Hermes: X-Webhook-Signature-V2 = hex HMAC-SHA256 de
// "<timestamp>.<body>", con X-Webhook-Timestamp.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hermesConfig struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret"`
	Kinds  []string `json:"kinds,omitempty"`
}

// hermesKinds: solo lo que toca al trabajo de la IA (lo que lanzó terminó o
// falló, o le cancelaron la tarea). La salud de la flota (PC apagada, freno,
// disco, servicios) es para el dueño y va solo a Telegram. "kinds" lo cambia.
var hermesKinds = []string{"work.failed", "job.done", "job.failed", "download.done", "download.failed"}

const hermesBatch = 5 * time.Second

type hermesSink struct {
	path   string
	logger *log.Logger
	client *http.Client

	mu      sync.Mutex
	pending []event
}

func hermesConfigPath(machinesPath string) string {
	return filepath.Join(filepath.Dir(machinesPath), "hermes.json")
}

func startHermes(p *panel, path string, logger *log.Logger) {
	h := &hermesSink{path: path, logger: logger, client: &http.Client{Timeout: 20 * time.Second}}
	if cfg, err := h.config(); err == nil {
		logger.Printf("hermes: avisos a %s", cfg.URL)
	}
	p.events.subscribe(h.add)
}

func (h *hermesSink) config() (hermesConfig, error) {
	var cfg hermesConfig
	data, err := os.ReadFile(h.path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", h.path, err)
	}
	if cfg.URL == "" || cfg.Secret == "" {
		return cfg, fmt.Errorf("%s necesita \"url\" y \"secret\"", h.path)
	}
	if len(cfg.Kinds) == 0 {
		cfg.Kinds = hermesKinds
	}
	return cfg, nil
}

// add junta los avisos de unos segundos en un solo envío.
func (h *hermesSink) add(e event) {
	cfg, err := h.config()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.logger.Printf("hermes: %v", err)
		}
		return
	}
	if !slices.Contains(cfg.Kinds, e.Kind) {
		return
	}
	h.mu.Lock()
	h.pending = append(h.pending, e)
	first := len(h.pending) == 1
	h.mu.Unlock()
	if first {
		time.AfterFunc(hermesBatch, h.flush)
	}
}

func (h *hermesSink) flush() {
	h.mu.Lock()
	list := h.pending
	h.pending = nil
	h.mu.Unlock()
	cfg, err := h.config()
	if err != nil || len(list) == 0 {
		return
	}
	slices.SortFunc(list, func(a, b event) int { return int(a.Seq - b.Seq) })
	lines := make([]string, len(list))
	for i, e := range list {
		lines[i] = "- " + strings.ReplaceAll(e.Text, "\n", " · ")
	}
	body, _ := json.Marshal(map[string]any{
		"event_type": "astrea",
		"text":       strings.Join(lines, "\n"),
		"events":     list,
	})
	for try := 0; try < 3; try++ {
		if err = h.post(cfg, body); err == nil {
			return
		}
		time.Sleep(time.Duration(try+1) * 10 * time.Second)
	}
	h.logger.Printf("hermes: no pude avisar (%d avisos): %v", len(list), err)
}

func (h *hermesSink) post(cfg hermesConfig, body []byte) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Timestamp", ts)
	req.Header.Set("X-Webhook-Signature-V2", hex.EncodeToString(mac.Sum(nil)))
	res, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
