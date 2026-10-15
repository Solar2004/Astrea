package main

import (
	"context"
	"encoding/json"
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
	Notes    []struct {
		Time     time.Time `json:"time"`
		Text     string    `json:"text,omitempty"`
		Progress string    `json:"progress,omitempty"`
	} `json:"notes,omitempty"`
}

// fleetWork gathers every machine's recent tasks, newest first. Machines that
// do not answer (or run an agent without work.*) are listed in errs.
func (p *panel) fleetWork(ctx context.Context, perMachine int) (tasks []workTask, errs map[string]string) {
	machines := p.reload()
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs = map[string]string{}
	tasks = []workTask{}
	for _, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res struct {
				Tasks []workTask `json:"tasks"`
			}
			_, err := p.call(ctx, m.Name, "work.list", map[string]any{"limit": perMachine}, &res, 8*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[m.Name] = err.Error()
				return
			}
			for _, t := range res.Tasks {
				t.Machine = m.Name
				t.Notes = nil // the list stays light; work.get has them
				tasks = append(tasks, t)
			}
		}()
	}
	wg.Wait()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Started.After(tasks[j].Started) })
	return tasks, errs
}

// handleWork:
//
//	GET /api/work                      → every machine's recent tasks, newest first
//	GET /api/work?machine=NAME         → that machine's tasks
//	GET /api/work?machine=NAME&id=ID   → one task with its notes and actions
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
	if id := q.Get("id"); id != "" {
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
		if code, err := p.call(r.Context(), machine, "work.get", map[string]any{"id": id, "limit": 500}, &res, 10*time.Second); err != nil {
			fail(w, code, err)
			return
		}
		res.Task.Machine = machine
		actions := make([]map[string]any, 0, len(res.Actions))
		for _, e := range res.Actions {
			item := map[string]any{"ts": e.Time, "method": e.Method, "ok": e.OK, "summary": auditSummary(e.Method, e.Args)}
			if e.Err != "" {
				item["error"] = e.Err
			}
			actions = append(actions, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": res.Task, "actions": actions})
		return
	}
	var res struct {
		Tasks []workTask `json:"tasks"`
	}
	if code, err := p.call(r.Context(), machine, "work.list", map[string]any{"limit": limit}, &res, 8*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	for i := range res.Tasks {
		res.Tasks[i].Machine = machine
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": res.Tasks})
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
		if t.Status == "running" {
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
