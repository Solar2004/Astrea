package main

// Control endpoints of the panel (kill switch, machines, activity, services).
// Every mutating endpoint is POST/DELETE and same-origin only (see sameOrigin).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/Solar2004/Astrea/internal/transport"
)

// mutating guards a handler: right method and same origin.
func mutating(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	ok := false
	for _, m := range methods {
		ok = ok || r.Method == m
	}
	if !ok {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use " + strings.Join(methods, "/")})
		return false
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request refused"})
		return false
	}
	return true
}

// call runs one capability on a named machine and decodes its result.
func (p *panel) call(ctx context.Context, machine, method string, args, out any, timeout time.Duration) (int, error) {
	client := p.client(machine)
	if client == nil {
		return http.StatusNotFound, errUnknownMachine
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := client.Call(ctx, method, args)
	if err != nil {
		if strings.Contains(err.Error(), "unknown method") {
			return http.StatusNotImplemented, fmt.Errorf("este agente no tiene %s: actualízalo", method)
		}
		return http.StatusBadGateway, err
	}
	if out != nil {
		if err := json.Unmarshal(res.Result, out); err != nil {
			return http.StatusBadGateway, err
		}
	}
	return http.StatusOK, nil
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// handleLock: POST /api/lock?machine=&locked=true|false — the kill switch.
func (p *panel) handleLock(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	locked, err := strconv.ParseBool(q.Get("locked"))
	if err != nil {
		fail(w, http.StatusBadRequest, errors.New("locked debe ser true o false"))
		return
	}
	var res struct {
		Locked bool `json:"locked"`
	}
	if code, err := p.call(r.Context(), q.Get("machine"), "admin.lock", map[string]any{"locked": locked}, &res, 10*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	p.logger.Printf("kill switch %s: locked=%v (desde el panel)", q.Get("machine"), res.Locked)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locked": res.Locked})
}

// handleAudit: GET /api/audit?machine=&limit=100 → recent activity.
func (p *panel) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	var res struct {
		Entries []struct {
			Time   string `json:"time"`
			Method string `json:"method"`
			Args   string `json:"args"`
			OK     bool   `json:"ok"`
			Err    string `json:"err"`
		} `json:"entries"`
	}
	if code, err := p.call(r.Context(), q.Get("machine"), "admin.audit", map[string]any{"limit": limit}, &res, 10*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	out := make([]map[string]any, 0, len(res.Entries))
	for _, e := range res.Entries {
		item := map[string]any{"ts": e.Time, "method": e.Method, "ok": e.OK, "summary": auditSummary(e.Method, e.Args)}
		if e.Err != "" {
			item["error"] = e.Err
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// auditSummary picks the one argument a person cares about for each call:
// the command run, the file touched, the service started….
func auditSummary(method, args string) string {
	var a map[string]any
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "name", "url", "text", "endpoint", "pid", "action", "locked"} {
		if v, ok := a[k]; ok {
			s := fmt.Sprint(v)
			if len(s) > 160 {
				s = s[:160] + "…"
			}
			return s
		}
	}
	return ""
}

// handleService: POST /api/service?machine=&name=&action=start|stop|restart.
func (p *panel) handleService(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		fail(w, http.StatusBadRequest, errors.New("falta ?name="))
		return
	}
	method := ""
	switch q.Get("action") {
	case "start", "restart":
		method = "service.start" // with only a name it (re)starts the existing service
	case "stop":
		method = "service.stop"
	default:
		fail(w, http.StatusBadRequest, errors.New("action debe ser start, stop o restart"))
		return
	}
	var res map[string]any
	if code, err := p.call(r.Context(), q.Get("machine"), method, map[string]any{"name": name, "wait_ready_sec": 20}, &res, 40*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	res["ok"] = true
	writeJSON(w, http.StatusOK, res)
}

// handleServiceLogs: GET /api/service/logs?machine=&name=&lines=200.
func (p *panel) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lines, _ := strconv.Atoi(q.Get("lines"))
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	var res map[string]any
	if code, err := p.call(r.Context(), q.Get("machine"), "service.logs", map[string]any{"name": q.Get("name"), "tail_lines": lines}, &res, 15*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- adding and removing machines ---

var (
	reWS        = regexp.MustCompile(`wss?://[A-Za-z0-9.\-:\[\]]+/ws`)
	reAddLine   = regexp.MustCompile(`add-machine\s+(\S+)\s+(\S+)\s+(\S+)`)
	reTokenFlag = regexp.MustCompile(`--token["'\s,\-]*["']?([A-Za-z0-9_\-]{16,})`)
	reTokenJSON = regexp.MustCompile(`"token"\s*:\s*"([A-Za-z0-9_\-]{16,})"`)
	reBadName   = regexp.MustCompile(`[^A-Za-z0-9_\-]+`)
)

// parseCard pulls name, ws URL and token out of whatever the user pasted:
// the installer's hub-config.txt, an `install.sh add-machine` line, a JSON
// machine entry or a plain "--ws … --token …" command line.
func parseCard(text string) (name, ws, token string) {
	if m := reAddLine.FindStringSubmatch(text); m != nil {
		name, token = m[1], m[3]
		ws = m[2]
		if !strings.Contains(ws, "://") {
			ws = "ws://" + ws + ":7700/ws"
		}
	}
	if ws == "" {
		ws = reWS.FindString(text)
	}
	if token == "" {
		if m := reTokenFlag.FindStringSubmatch(text); m != nil {
			token = m[1]
		} else if m := reTokenJSON.FindStringSubmatch(text); m != nil {
			token = m[1]
		}
	}
	if name == "" {
		var j struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(text), &j) == nil {
			name = j.Name
		}
	}
	return name, ws, token
}

// cleanName turns a hostname into a valid machine name.
func cleanName(s string) string {
	s = strings.Trim(reBadName.ReplaceAllString(s, "-"), "-")
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// handleMachines: POST adds/replaces a machine from a pasted card, DELETE
// ?name= removes one. Both edit the config file the panel and hub share.
func (p *panel) handleMachines(w http.ResponseWriter, r *http.Request) {
	if p.ws != "" {
		fail(w, http.StatusConflict, errors.New("el panel se lanzó con --ws: no usa archivo de máquinas"))
		return
	}
	switch r.Method {
	case http.MethodPost:
		if !mutating(w, r, http.MethodPost) {
			return
		}
		p.addMachine(w, r)
	case http.MethodDelete:
		if !mutating(w, r, http.MethodDelete) {
			return
		}
		name := r.URL.Query().Get("name")
		removed := false
		err := p.editMachines(func(ms []Machine) []Machine {
			out := ms[:0]
			for _, m := range ms {
				if m.Name == name {
					removed = true
					continue
				}
				out = append(out, m)
			}
			return out
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if !removed {
			fail(w, http.StatusNotFound, errUnknownMachine)
			return
		}
		p.reload()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name})
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("use POST o DELETE"))
	}
}

func (p *panel) addMachine(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	text := string(body)
	var req struct{ Text, Name string }
	if json.Unmarshal(body, &req) == nil && req.Text != "" {
		text = req.Text // {"text": "...", "name": "..."} also accepted
	}
	name, ws, token := parseCard(text)
	if req.Name != "" {
		name = req.Name
	}
	if q := r.URL.Query().Get("name"); q != "" {
		name = q
	}
	if ws != "" && token != "" && len(token) < 16 {
		fail(w, http.StatusBadRequest, errors.New("el token parece incompleto (tiene "+strconv.Itoa(len(token))+" caracteres; los de Astrea tienen 64): copia la ficha completa"))
		return
	}
	if ws == "" || token == "" {
		fail(w, http.StatusBadRequest, errors.New("no encuentro la dirección (ws://…/ws) y el token en lo que pegaste; copia la ficha completa de hub-config.txt"))
		return
	}

	// Probe it before saving: a wrong IP or token should fail here, not later.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	client, err := transport.Dial(ctx, ws, token)
	if err != nil {
		msg := "no conecta con " + ws + ": ¿está encendida, con Tailscale y el agente en marcha?"
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			msg = "el equipo responde pero rechaza el token: copia la ficha otra vez"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": msg, "detail": err.Error()})
		return
	}
	defer client.Close()
	res, err := client.Call(ctx, "sys.info", map[string]any{})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "conecta pero no responde: " + err.Error()})
		return
	}
	var info struct {
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
	}
	_ = json.Unmarshal(res.Result, &info)
	if name == "" {
		name = info.Hostname
	}
	name = cleanName(name)
	m := Machine{Name: name, WS: ws, Token: token}
	if err := fleet.Validate([]fleet.Machine{fleet.Machine(m)}); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	replaced := false
	err = p.editMachines(func(ms []Machine) []Machine {
		out := ms[:0]
		for _, x := range ms {
			if x.Name == m.Name || x.WS == m.WS {
				replaced = true
				continue
			}
			out = append(out, x)
		}
		return append(out, m)
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	p.reload()
	p.logger.Printf("máquina %s añadida desde el panel (%s)", m.Name, m.WS)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": m.Name, "ws": m.WS, "os": info.OS, "hostname": info.Hostname, "replaced": replaced,
		"note": "el panel ya la muestra; reinicia Hermes para que vea sus herramientas"})
}

// editMachines rewrites the machines file through fn, keeping any other
// top-level keys, atomically and readable only by the user (it holds tokens).
func (p *panel) editMachines(fn func([]Machine) []Machine) error {
	p.editMu.Lock()
	defer p.editMu.Unlock()
	doc := map[string]json.RawMessage{}
	var cfg panelConfig
	data, err := os.ReadFile(p.cfgPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s no es JSON válido: %w", p.cfgPath, err)
		}
		_ = json.Unmarshal(data, &cfg)
	}
	ms := fn(cfg.Machines)
	if ms == nil {
		ms = []Machine{}
	}
	raw, _ := json.Marshal(ms)
	doc["machines"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.cfgPath), 0o700); err != nil {
		return err
	}
	tmp := p.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.cfgPath)
}
