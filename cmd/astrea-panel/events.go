// Avisos de la flota: el panel sondea los equipos, detecta los cambios
// (encendido/apagado, freno, tarea/job/descarga terminados, servicio caído,
// alguien se sentó, umbrales) y los guarda en un registro con número de
// secuencia. De ahí beben Telegram (telegram.go), Hermes (hermes.go), la web
// y el hub (GET /api/events, long-poll con ?since=&wait=).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/supervise"
)

// event es un aviso. Text es texto plano (Hermes, web); HTML va a Telegram.
type event struct {
	Seq     int64  `json:"seq"`
	Time    int64  `json:"time"` // unix ms
	Machine string `json:"machine"`
	// Kind: online, offline, lock, unlock, work.done, work.failed, job.done,
	// job.failed, download.done, download.failed, service.failed, busy, free,
	// gpu.hot, disk.full, ram.full.
	Kind  string `json:"kind"`
	Level string `json:"level"` // good, info, warn, bad
	Text  string `json:"text"`
	Ref   string `json:"ref,omitempty"` // id de la tarea/job/descarga o nombre del servicio

	html   string // Telegram
	button string // botón de Telegram: "m" (equipo) o "t" (tareas)
}

const eventKeep = 300

type eventLog struct {
	mu   sync.Mutex
	list []event
	seq  int64
	wake chan struct{} // se cierra y renueva en cada aviso (long-poll)
	subs []func(event)
}

func newEventLog() *eventLog {
	// Empieza en el reloj: así un cliente con un since de antes de reiniciar
	// el panel no se pierde los avisos nuevos.
	return &eventLog{seq: time.Now().UnixMilli(), wake: make(chan struct{})}
}

// subscribe llama a fn (en su propia goroutine por aviso) con cada aviso nuevo.
func (l *eventLog) subscribe(fn func(event)) {
	l.mu.Lock()
	l.subs = append(l.subs, fn)
	l.mu.Unlock()
}

func (l *eventLog) add(e event) {
	l.mu.Lock()
	l.seq++
	e.Seq, e.Time = l.seq, time.Now().UnixMilli()
	if e.Text == "" {
		e.Text = plainText(e.html)
	}
	l.list = append(l.list, e)
	if len(l.list) > eventKeep {
		l.list = l.list[len(l.list)-eventKeep:]
	}
	close(l.wake)
	l.wake = make(chan struct{})
	subs := l.subs
	l.mu.Unlock()
	for _, fn := range subs {
		go fn(e)
	}
}

// since devuelve los avisos posteriores a seq (todos si seq ≤ 0) y el último número.
func (l *eventLog) since(seq int64, machine string) ([]event, int64, chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []event{}
	for _, e := range l.list {
		if e.Seq > seq && (machine == "" || e.Machine == machine) {
			out = append(out, e)
		}
	}
	return out, l.seq, l.wake
}

// handleEvents: GET /api/events?since=<seq>&wait=<s>&machine=<nombre>.
// Con wait espera hasta que haya algo nuevo (máx. 120 s).
func (p *panel) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	wait, _ := strconv.Atoi(q.Get("wait"))
	wait = min(max(wait, 0), 120)
	machine := q.Get("machine")
	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		list, last, wake := p.events.since(since, machine)
		if len(list) > 0 || wait == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"events": list, "last": last})
			return
		}
		select {
		case <-wake:
		case <-deadline:
			wait = 0
		case <-r.Context().Done():
			return
		}
	}
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

func plainText(s string) string { return html.UnescapeString(tagRE.ReplaceAllString(s, "")) }

// --- vigilancia ---

// machineWatch es lo que se sabe de un equipo entre rondas.
type machineWatch struct {
	online    bool
	fails     int       // fallos seguidos (para no avisar por un timeout suelto)
	since     time.Time // desde cuándo está en el estado actual
	locked    bool
	ramSince  time.Time         // desde cuándo la RAM está ≥ 95 %
	services  map[string]string // último estado de cada servicio
	work      string            // tarea en curso (id)
	busy      bool              // alguien la usa
	busyFlips int               // rondas seguidas con el uso cambiado
	jobs      map[string]string // id → estado
	downloads map[string]string // id → estado
}

const alertCooldown = 30 * time.Minute

// watch sondea la flota cada interval y registra los cambios. La primera vez
// que ve un equipo solo fija su línea base.
func (p *panel) watch(interval time.Duration) {
	state := map[string]*machineWatch{}
	sent := map[string]time.Time{} // equipo|métrica → último aviso de umbral
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for ; ; <-tick.C {
		list := p.pollAll()
		now := time.Now()
		seen := map[string]bool{}
		emit := func(name, kind, level, text string) {
			p.events.add(event{Machine: name, Kind: kind, Level: level, html: text, button: "m"})
		}
		threshold := func(name, metric, kind, text string) {
			k := name + "|" + metric
			if now.Sub(sent[k]) < alertCooldown {
				return
			}
			sent[k] = now
			emit(name, kind, "warn", text)
		}

		for _, s := range list {
			name := s.Name
			seen[name] = true
			c := cardFrom(s)
			w := state[name]
			if w == nil {
				w = &machineWatch{online: s.Online, since: now, locked: c.Locked, busy: c.Label == "En uso",
					services: map[string]string{}, jobs: map[string]string{}, downloads: map[string]string{}}
				if t, ok := currentWork(s); ok {
					w.work = t.ID
				}
				state[name] = w
				fillServices(w, s, nil)
				if s.Online {
					p.watchJobs(name, w, false)
				}
				continue // primera vez que lo vemos: sin aviso
			}
			// Encendido / apagado (dos fallos seguidos antes de darlo por apagado).
			switch {
			case s.Online:
				w.fails = 0
				if !w.online {
					emit(name, "online", "good", fmt.Sprintf("🟢 <b>%s</b> se encendió (estuvo apagado %s)", esc(name), ago(now.Sub(w.since))))
					w.online, w.since = true, now
				}
			case w.online:
				if w.fails++; w.fails >= 2 {
					emit(name, "offline", "bad", fmt.Sprintf("🔴 <b>%s</b> se apagó o perdió conexión (estuvo encendido %s)", esc(name), ago(now.Sub(w.since))))
					w.online, w.since = false, now
				}
			}
			if !s.Online {
				continue
			}
			// Freno.
			if c.Locked != w.locked {
				if c.Locked {
					emit(name, "lock", "bad", fmt.Sprintf("🔒 <b>%s</b>: freno de emergencia <b>activado</b>. La IA no puede hacer nada ahí.", esc(name)))
				} else {
					emit(name, "unlock", "good", fmt.Sprintf("🔓 <b>%s</b>: freno de emergencia <b>desactivado</b>. La IA vuelve a tener control.", esc(name)))
				}
				w.locked = c.Locked
			}
			// Alguien se sentó / la dejó libre (dos rondas seguidas, para no
			// avisar por un ratón que se movió una vez).
			if busy := c.Label == "En uso"; busy != w.busy {
				if w.busyFlips++; w.busyFlips >= 2 {
					w.busy, w.busyFlips = busy, 0
					if busy {
						emit(name, "busy", "warn", fmt.Sprintf("🧑 <b>%s</b>: alguien se sentó a usarla", esc(name)))
					} else {
						emit(name, "free", "good", fmt.Sprintf("🪑 <b>%s</b> quedó libre", esc(name)))
					}
				}
			} else {
				w.busyFlips = 0
			}
			// Tareas: avisa cuando termina la que estaba en curso.
			cur, _ := currentWork(s)
			if w.work != "" && cur.ID != w.work {
				if t, text, ok := p.finishedWork(name, w.work); ok {
					kind, level := "work.done", "good"
					if t.Status != "done" {
						kind, level = "work.failed", "bad"
					}
					p.events.add(event{Machine: name, Kind: kind, Level: level, Ref: t.ID, html: text, button: "t"})
				}
			}
			w.work = cur.ID
			p.watchJobs(name, w, true)
			// Umbrales.
			if c.Stats != nil {
				for _, g := range c.Stats.GPUs {
					if g.TempC != nil && *g.TempC >= 85 {
						threshold(name, "gpu-temp", "gpu.hot", fmt.Sprintf("🌡 <b>%s</b>: la gráfica está a %.0f °C", esc(name), *g.TempC))
					}
				}
				for _, d := range c.Stats.Disks {
					if d.UsedPct >= 90 {
						threshold(name, "disk|"+d.Mount, "disk.full", fmt.Sprintf("💾 <b>%s</b>: el disco %s está al %.0f %% (quedan %s)", esc(name), esc(d.Mount), d.UsedPct, cardBytes(d.Total-d.Used)))
					}
				}
				if val(c.Stats.Memory.UsedPct) >= 95 {
					if w.ramSince.IsZero() {
						w.ramSince = now
					} else if now.Sub(w.ramSince) >= 2*time.Minute {
						threshold(name, "ram", "ram.full", fmt.Sprintf("🧠 <b>%s</b>: la memoria lleva %s al %.0f %%", esc(name), ago(now.Sub(w.ramSince)), val(c.Stats.Memory.UsedPct)))
					}
				} else {
					w.ramSince = time.Time{}
				}
			}
			fillServices(w, s, func(svc string) {
				p.events.add(event{Machine: name, Kind: "service.failed", Level: "bad", Ref: svc, button: "m",
					html: fmt.Sprintf("⚙️ <b>%s</b>: el servicio <b>%s</b> ha fallado", esc(name), esc(svc))})
			})
		}
		for name := range state {
			if !seen[name] {
				delete(state, name)
			}
		}
	}
}

// pollAll consulta todos los equipos a la vez.
func (p *panel) pollAll() []machineStatus {
	machines := p.reload()
	out := make([]machineStatus, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p.poll(context.Background(), m)
		}()
	}
	wg.Wait()
	return out
}

// fillServices actualiza el estado de los servicios y llama a failed por cada
// uno que acaba de pasar a "failed".
func fillServices(w *machineWatch, s machineStatus, failed func(string)) {
	var res struct {
		Services []tgService `json:"services"`
	}
	if len(s.Services) == 0 || json.Unmarshal(s.Services, &res) != nil {
		return
	}
	for _, x := range res.Services {
		if failed != nil && x.State == "failed" && w.services[x.Name] != "failed" && w.services[x.Name] != "" {
			failed(x.Name)
		}
		w.services[x.Name] = x.State
	}
}

// watchJobs registra los jobs y descargas que acaban de terminar. Con
// report=false solo apunta el estado (línea base).
func (p *panel) watchJobs(name string, w *machineWatch, report bool) {
	ctx := context.Background()
	var jobs struct {
		Jobs []supervise.Job `json:"jobs"`
	}
	if _, err := p.call(ctx, name, "job.status", map[string]any{}, &jobs, 6*time.Second); err == nil {
		for _, j := range jobs.Jobs {
			prev, known := w.jobs[j.ID]
			w.jobs[j.ID] = j.State
			if !report || j.State == supervise.JobRunning || (known && prev != supervise.JobRunning) || (!known && time.Since(j.EndedAt) > time.Minute) {
				continue
			}
			label := j.Label
			if label == "" {
				label = clipRunes(j.Command.Command, 60)
			}
			e := event{Machine: name, Kind: "job.done", Level: "good", Ref: j.ID, button: "m"}
			e.html = fmt.Sprintf("✅ <b>%s</b>: terminó el job <b>%s</b> (%s)", esc(name), esc(label), ago(time.Duration(j.DurationSec)*time.Second))
			if j.State != supervise.JobSucceeded {
				e.Kind, e.Level = "job.failed", "bad"
				e.html = fmt.Sprintf("❌ <b>%s</b>: el job <b>%s</b> acabó mal (%s)", esc(name), esc(label), esc(jobReason(j)))
			}
			p.events.add(e)
		}
	}
	var dl struct {
		Downloads []struct {
			ID         string    `json:"id"`
			Dest       string    `json:"dest"`
			State      string    `json:"state"`
			Error      string    `json:"error"`
			FinishedAt time.Time `json:"finished_at"`
		} `json:"downloads"`
	}
	if _, err := p.call(ctx, name, "net.download.status", map[string]any{}, &dl, 6*time.Second); err == nil {
		for _, d := range dl.Downloads {
			prev, known := w.downloads[d.ID]
			w.downloads[d.ID] = d.State
			ended := d.State == "done" || d.State == "failed" || d.State == "canceled"
			if !report || !ended || (known && prev != "running" && prev != "verifying") || (!known && time.Since(d.FinishedAt) > time.Minute) {
				continue
			}
			e := event{Machine: name, Kind: "download.done", Level: "good", Ref: d.ID, button: "m",
				html: fmt.Sprintf("📥 <b>%s</b>: descarga terminada → %s", esc(name), esc(d.Dest))}
			switch d.State {
			case "canceled":
				e.Kind, e.Level = "download.failed", "bad"
				e.html = fmt.Sprintf("⏹ <b>%s</b>: se canceló la descarga a %s", esc(name), esc(d.Dest))
			case "failed":
				e.Kind, e.Level = "download.failed", "bad"
				e.html = fmt.Sprintf("❌ <b>%s</b>: falló la descarga a %s: %s", esc(name), esc(d.Dest), esc(clipRunes(d.Error, 160)))
			}
			p.events.add(e)
		}
	}
}

func jobReason(j supervise.Job) string {
	switch {
	case j.State == supervise.JobCanceled:
		return "cancelado"
	case j.State == supervise.JobTimedOut:
		return "se pasó de tiempo"
	case j.Error != "":
		return clipRunes(j.Error, 120)
	case j.ExitCode != nil:
		return fmt.Sprintf("código %d", *j.ExitCode)
	}
	return j.State
}
