package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// workTask is a task as the agents report it (work.list / work.get).
type workTask struct {
	Machine  string     `json:"machine,omitempty"`
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Detail   string     `json:"detail,omitempty"`
	Status   string     `json:"status"`
	Progress string     `json:"progress,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	LastSeen time.Time  `json:"last_activity"`
	Actions  int        `json:"actions"`
	Errors   int        `json:"errors"`
	// Stale: copied from the panel's cache because the machine did not answer.
	Stale    bool       `json:"stale,omitempty"`
	CachedAt *time.Time `json:"cached_at,omitempty"`
	Notes    []struct {
		Time     time.Time `json:"time"`
		Text     string    `json:"text,omitempty"`
		Progress string    `json:"progress,omitempty"`
	} `json:"notes,omitempty"`
}

// workTimeout: short for machines that have not answered lately, so one PC
// that is off does not hold up the whole list.
func (p *panel) workTimeout(machine string) time.Duration {
	p.mu.Lock()
	seen := p.lastSeen[machine]
	p.mu.Unlock()
	if time.Since(time.UnixMilli(seen)) < time.Minute {
		return 8 * time.Second
	}
	return 2 * time.Second
}

const errNoAnswer = "sin conexión"

// machineWork lists a machine's recent tasks, falling back to the panel's
// cached copy (stale) when it does not answer. err is set whenever the live
// list could not be read.
func (p *panel) machineWork(ctx context.Context, machine string, limit int) ([]workTask, int, error) {
	var res struct {
		Tasks []workTask `json:"tasks"`
	}
	code, err := p.call(ctx, machine, "work.list", map[string]any{"limit": limit}, &res, p.workTimeout(machine))
	if err == nil {
		for i := range res.Tasks {
			res.Tasks[i].Machine = machine
		}
		p.work.putList(machine, res.Tasks)
		p.mu.Lock()
		p.lastSeen[machine] = time.Now().UnixMilli()
		p.mu.Unlock()
		return res.Tasks, http.StatusOK, nil
	}
	if code == http.StatusBadGateway {
		err = errors.New(errNoAnswer)
	}
	tasks, _ := p.work.list(machine)
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, code, err
}

// fleetWork gathers every machine's recent tasks, newest first. Machines that
// do not answer (or run an agent without work.*) are listed in errs, with
// their last known tasks marked stale.
func (p *panel) fleetWork(ctx context.Context, perMachine int) (tasks []workTask, errs map[string]string) {
	machines := p.reload()
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs = map[string]string{}
	tasks = []workTask{}
	keep := map[string]bool{}
	for _, m := range machines {
		keep[m.Name] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := p.machineWork(ctx, m.Name, perMachine)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[m.Name] = err.Error()
			}
			for _, t := range got {
				t.Notes = nil // the list stays light; work.get has them
				tasks = append(tasks, t)
			}
		}()
	}
	wg.Wait()
	p.work.forget(keep)
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Started.After(tasks[j].Started) })
	return tasks, errs
}

type workAction struct {
	TS      string `json:"ts"`
	Method  string `json:"method"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

type workDetail struct {
	Task    workTask     `json:"task"`
	Actions []workAction `json:"actions"`
}

// handleWork:
//
//	GET /api/work                      → every machine's recent tasks, newest first
//	GET /api/work?machine=NAME         → that machine's tasks
//	GET /api/work?machine=NAME&id=ID   → one task with its notes and actions
//
// When a machine does not answer, its last known data is served with
// stale:true (and the reason in "error").
func (p *panel) handleWork(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 30
	}
	machine := q.Get("machine")
	if machine == "" {
		tasks, errs := p.fleetWork(r.Context(), limit)
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "errors": errs})
		return
	}
	if p.client(machine) == nil {
		fail(w, http.StatusNotFound, errUnknownMachine)
		return
	}
	if id := q.Get("id"); id != "" {
		d, code, err := p.workDetail(r.Context(), machine, id)
		if err == nil {
			writeJSON(w, http.StatusOK, d)
			return
		}
		if c, ok := p.work.detail(machine, id); ok {
			var cached workDetail
			if json.Unmarshal(c.Detail, &cached) == nil {
				cached.Task.Stale, cached.Task.CachedAt = true, &c.At
				writeJSON(w, http.StatusOK, map[string]any{"task": cached.Task, "actions": cached.Actions, "stale": true, "error": err.Error()})
				return
			}
		}
		fail(w, code, err)
		return
	}
	tasks, code, err := p.machineWork(r.Context(), machine, limit)
	if err != nil && len(tasks) == 0 {
		fail(w, code, err)
		return
	}
	out := map[string]any{"tasks": tasks}
	if err != nil {
		out["stale"], out["error"] = true, err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// workDetail reads one task with its actions from the agent (and caches it).
func (p *panel) workDetail(ctx context.Context, machine, id string) (workDetail, int, error) {
	var res struct {
		Task    workTask `json:"task"`
		Actions []struct {
			Time   string `json:"time"`
			Method string `json:"method"`
			Args   string `json:"args"`
			OK     bool   `json:"ok"`
			Err    string `json:"err"`
		} `json:"actions"`
	}
	code, err := p.call(ctx, machine, "work.get", map[string]any{"id": id, "limit": 500}, &res, p.workTimeout(machine))
	if err != nil {
		if code == http.StatusBadGateway {
			err = errors.New(errNoAnswer)
		}
		return workDetail{}, code, err
	}
	d := workDetail{Task: res.Task, Actions: make([]workAction, 0, len(res.Actions))}
	d.Task.Machine = machine
	for _, e := range res.Actions {
		d.Actions = append(d.Actions, workAction{TS: e.Time, Method: e.Method, OK: e.OK, Summary: auditSummary(e.Method, e.Args), Error: e.Err})
	}
	p.work.putDetail(machine, id, d)
	p.work.patch(machine, d.Task)
	return d, http.StatusOK, nil
}

// handleWorkCancel: POST /api/work/cancel?machine=&id= closes that task as
// cancelled if it is still the machine's current one. The AI's next
// "continue" on it is refused with the reason, so it stops.
func (p *panel) handleWorkCancel(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	machine, id := q.Get("machine"), q.Get("id")
	if id == "" {
		fail(w, http.StatusBadRequest, errors.New("falta ?id="))
		return
	}
	var admin struct {
		Work *workTask `json:"work"`
	}
	if code, err := p.call(r.Context(), machine, "admin.status", map[string]any{}, &admin, 8*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	if admin.Work == nil || admin.Work.ID != id {
		fail(w, http.StatusConflict, errors.New("esa tarea ya no está en curso"))
		return
	}
	var t workTask
	args := map[string]any{"id": id, "status": "cancelled", "summary": "Cancelada desde el panel"}
	if code, err := p.call(r.Context(), machine, "work.end", args, &t, 8*time.Second); err != nil {
		if strings.Contains(err.Error(), "no longer in progress") {
			code, err = http.StatusConflict, errors.New("esa tarea ya no está en curso")
		}
		fail(w, code, err)
		return
	}
	t.Machine = machine
	p.work.patch(machine, t)
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

// workStatus: emoji and Spanish label of a task state.
func workStatus(status string) (string, string) {
	switch status {
	case "running":
		return "▶️", "en curso"
	case "done":
		return "✅", "terminada"
	case "failed":
		return "❌", "falló"
	case "cancelled":
		return "⏹", "cancelada"
	case "replaced":
		return "↪️", "cerrada al empezar otra"
	case "abandoned":
		return "💤", "abandonada (sin actividad)"
	}
	return "•", status
}

// duration of a task so far, or in total once ended.
func (t workTask) duration() time.Duration {
	end := time.Now()
	if t.Ended != nil {
		end = *t.Ended
	}
	return end.Sub(t.Started)
}

// workLine: "▶️ <b>Generar vídeos</b> · 3/5 · 12 min · 34 acciones".
func workLine(t workTask, withMachine bool) string {
	emoji, _ := workStatus(t.Status)
	line := emoji + " "
	if withMachine && t.Machine != "" {
		line += esc(t.Machine) + " · "
	}
	line += "<b>" + esc(t.Title) + "</b>"
	if t.Progress != "" {
		line += " · " + esc(t.Progress)
	}
	line += " · " + ago(t.duration())
	if t.Actions > 0 {
		line += fmt.Sprintf(" · %d %s", t.Actions, plural(t.Actions, "acción", "acciones"))
	}
	if t.Errors > 0 {
		line += fmt.Sprintf(" (%d ✗)", t.Errors)
	}
	return line
}

// currentWork is the task in a machine status (admin.status "work"), if any.
func currentWork(s machineStatus) (workTask, bool) {
	var admin struct {
		Work *workTask `json:"work"`
	}
	if !s.Online || json.Unmarshal(s.Admin, &admin) != nil || admin.Work == nil {
		return workTask{}, false
	}
	admin.Work.Machine = s.Name
	return *admin.Work, true
}

// workCaption: one machine's current task and the recent ones.
func (b *telegramBot) workCaption(name string) (string, any) {
	name = b.resolve(name)
	back := inline([]map[string]string{btn("🔄 Actualizar", "t:"+name), btn("← Equipo", "m:"+name)})
	var res struct {
		Tasks []workTask `json:"tasks"`
	}
	if _, err := b.p.call(context.Background(), name, "work.list", map[string]any{"limit": 8}, &res, 10*time.Second); err != nil {
		return fmt.Sprintf("📋 <b>%s</b>: no pude leer las tareas — %s", esc(name), esc(err.Error())), back
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 <b>%s</b> · tareas de la IA\n", esc(name))
	if len(res.Tasks) == 0 {
		sb.WriteString("\nNinguna todavía.")
	}
	for _, t := range res.Tasks {
		item := "\n" + workLine(t, false)
		if t.Status == "running" && t.Detail != "" {
			item += "\n    " + esc(clipRunes(t.Detail, 100))
		}
		if t.Summary != "" {
			item += "\n    <i>" + esc(clipRunes(t.Summary, 100)) + "</i>"
		}
		if utf8.RuneCountInString(sb.String()+item) > 1000 { // límite del pie de foto: 1024
			break
		}
		sb.WriteString(item)
	}
	return sb.String(), back
}

// fleetWorkText: what every machine is doing now, and the latest tasks.
func (b *telegramBot) fleetWorkText() string {
	tasks, errs := b.p.fleetWork(context.Background(), 8)
	var now, recent []string
	for _, t := range tasks {
		if t.Status == "running" && !t.Stale {
			now = append(now, workLine(t, true))
		} else if len(recent) < 8 {
			recent = append(recent, workLine(t, true))
		}
	}
	var sb strings.Builder
	sb.WriteString("📋 <b>Tareas de la IA</b>\n")
	if len(now) == 0 {
		sb.WriteString("\nAhora mismo no está trabajando en ningún equipo.")
	} else {
		sb.WriteString("\n<b>Ahora</b>\n" + strings.Join(now, "\n"))
	}
	if len(recent) > 0 {
		sb.WriteString("\n\n<b>Últimas</b>\n" + strings.Join(recent, "\n"))
	}
	if len(errs) > 0 {
		names := make([]string, 0, len(errs))
		for n := range errs {
			names = append(names, esc(n))
		}
		sort.Strings(names)
		sb.WriteString("\n\n<i>Sin datos de: " + strings.Join(names, ", ") + "</i>")
	}
	return sb.String()
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// finishedWork: the alert for a task that just stopped being current, or
// false if it does not deserve one (replaced by the next task, unreadable).
func (b *telegramBot) finishedWork(machine, id string) (string, bool) {
	var res struct {
		Task workTask `json:"task"`
	}
	if _, err := b.p.call(context.Background(), machine, "work.get", map[string]any{"id": id, "limit": 1}, &res, 8*time.Second); err != nil {
		return "", false
	}
	t := res.Task
	emoji, label := workStatus(t.Status)
	switch t.Status {
	case "done", "failed", "cancelled", "abandoned":
	default:
		return "", false
	}
	text := fmt.Sprintf("%s <b>%s</b> · %s: <b>%s</b>\n⏱ %s", emoji, esc(machine), label, esc(t.Title), ago(t.duration()))
	if t.Actions > 0 {
		text += fmt.Sprintf(" · %d %s", t.Actions, plural(t.Actions, "acción", "acciones"))
	}
	if t.Summary != "" {
		text += "\n<i>" + esc(clipRunes(t.Summary, 600)) + "</i>"
	}
	return text, true
}
