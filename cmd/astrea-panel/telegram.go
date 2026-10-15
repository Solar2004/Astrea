// Aviso por Telegram: el panel vigila la flota y avisa cuando una PC se apaga
// o se enciende, y responde a un botón "📊 Estado" con cuántas hay activas.
// Bajo el estado salen botones por PC para ver sus stats o su pantalla.
//
// Se activa solo si existe telegram.json junto al archivo de máquinas:
//
//	{"token": "<token de @BotFather>", "user": "TuUsuario"}
//
// Telegram no deja a un bot escribir primero: el usuario tiene que mandarle
// /start una vez. Ese chat_id se guarda en el mismo telegram.json. Solo se
// atiende al username configurado; el resto recibe "bot privado".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	btnStatus = "📊 Estado"
	btnOnline = "🟢 Activas"
)

type telegramConfig struct {
	Token    string `json:"token"`
	User     string `json:"user"`                   // username autorizado, sin @
	ChatID   int64  `json:"chat_id,omitempty"`      // se aprende con /start
	Interval int    `json:"interval_sec,omitempty"` // cada cuánto se vigila (def. 20)
}

type telegramBot struct {
	p      *panel
	path   string
	logger *log.Logger
	client *http.Client

	mu  sync.Mutex
	cfg telegramConfig
}

// machineWatch es lo que se sabe de una PC entre rondas de vigilancia.
type machineWatch struct {
	online bool
	fails  int       // fallos seguidos (para no avisar por un timeout suelto)
	since  time.Time // desde cuándo está en el estado actual
}

// startTelegram arranca el bot si hay telegram.json; si no, no hace nada.
func startTelegram(p *panel, path string, logger *log.Logger) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		logger.Printf("telegram: %v", err)
		return
	}
	var cfg telegramConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		logger.Printf("telegram: %s: %v", path, err)
		return
	}
	if cfg.Token == "" || cfg.User == "" {
		logger.Printf("telegram: %s necesita \"token\" y \"user\"", path)
		return
	}
	cfg.User = strings.TrimPrefix(cfg.User, "@")
	if cfg.Interval <= 0 {
		cfg.Interval = 20
	}
	b := &telegramBot{p: p, path: path, logger: logger, cfg: cfg, client: &http.Client{Timeout: 70 * time.Second}}
	if cfg.ChatID == 0 {
		logger.Printf("telegram: activo; @%s tiene que mandarle /start al bot para recibir avisos", cfg.User)
	} else {
		logger.Printf("telegram: activo, avisando a @%s", cfg.User)
	}
	go b.listen()
	go b.watch()
}

func telegramConfigPath(machinesPath string) string {
	return filepath.Join(filepath.Dir(machinesPath), "telegram.json")
}

// --- API de Telegram ---

func (b *telegramBot) call(ctx context.Context, method string, req, out any) error {
	body, _ := json.Marshal(req)
	u := "https://api.telegram.org/bot" + b.cfg.Token + "/" + method
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(hreq)
	if err != nil {
		// *url.Error lleva la URL, que contiene el token: no la dejamos llegar al log.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if !r.OK {
		return fmt.Errorf("%s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func keyboard() map[string]any {
	return map[string]any{
		"keyboard":        [][]map[string]string{{{"text": btnStatus}, {"text": btnOnline}}},
		"resize_keyboard": true,
		"is_persistent":   true,
	}
}

// send manda texto HTML; markup nil = el teclado fijo de abajo.
func (b *telegramBot) send(chatID int64, text string, markup any) {
	if markup == nil {
		markup = keyboard()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := b.call(ctx, "sendMessage", map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": markup,
	}, nil)
	if err != nil {
		b.logger.Printf("telegram: %v", err)
	}
}

// notify manda un aviso al usuario, si ya se presentó con /start.
func (b *telegramBot) notify(text string) {
	b.mu.Lock()
	chatID := b.cfg.ChatID
	b.mu.Unlock()
	if chatID != 0 {
		b.send(chatID, text, nil)
	}
}

// sendPhoto sube una imagen (multipart: sendPhoto no acepta bytes en JSON).
func (b *telegramBot) sendPhoto(chatID int64, img []byte, mime, caption string) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", fmt.Sprint(chatID))
	_ = mw.WriteField("caption", caption)
	_ = mw.WriteField("parse_mode", "HTML")
	name := "pantalla.jpg"
	if mime == "image/png" {
		name = "pantalla.png"
	}
	fw, _ := mw.CreateFormFile("photo", name)
	_, _ = fw.Write(img)
	_ = mw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+b.cfg.Token+"/sendPhoto", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("sendPhoto: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	data, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(data, &r) != nil || !r.OK {
		return fmt.Errorf("sendPhoto: %s", r.Description)
	}
	return nil
}

// --- comandos ---

// listen recibe mensajes por long polling y responde a los botones.
func (b *telegramBot) listen() {
	offset := 0
	for {
		var updates []struct {
			UpdateID int `json:"update_id"`
			Message  *struct {
				Text string `json:"text"`
				From struct {
					Username string `json:"username"`
				} `json:"from"`
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
			} `json:"message"`
			Callback *struct {
				ID   string `json:"id"`
				Data string `json:"data"`
				From struct {
					Username string `json:"username"`
				} `json:"from"`
				Message struct {
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"callback_query"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		cancel()
		if err != nil {
			b.logger.Printf("telegram: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if c := u.Callback; c != nil {
				go b.handleButton(c.ID, c.Message.Chat.ID, c.From.Username, c.Data)
				continue
			}
			if u.Message == nil || u.Message.Chat.Type != "private" {
				continue
			}
			go b.handle(u.Message.Chat.ID, u.Message.From.Username, strings.TrimSpace(u.Message.Text))
		}
	}
}

func (b *telegramBot) handle(chatID int64, username, text string) {
	if !strings.EqualFold(username, b.cfg.User) {
		b.send(chatID, "Este bot es privado.", nil)
		return
	}
	b.rememberChat(chatID)
	fields := strings.Fields(text)
	cmd, arg := "", ""
	if len(fields) > 0 {
		cmd = strings.ToLower(strings.SplitN(fields[0], "@", 2)[0]) // /stats@bot → /stats
		arg = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	}
	switch {
	case text == btnStatus || cmd == "/estado" || cmd == "/status":
		b.sendStatus(chatID, false)
	case text == btnOnline || cmd == "/activas":
		b.sendStatus(chatID, true)
	case cmd == "/stats" && arg != "":
		b.send(chatID, b.machineText(arg), nil)
	case cmd == "/pantalla" && arg != "":
		b.screen(chatID, arg)
	case cmd == "/stats" || cmd == "/pantalla":
		b.sendStatus(chatID, true)
	default:
		b.send(chatID, "Hola 👋 Te aviso aquí cuando una PC de Astrea se apague o se encienda.\n\n"+
			"• <b>📊 Estado</b> / <b>🟢 Activas</b>: la flota, con botones por PC para ver stats o pantalla\n"+
			"• <code>/stats NOMBRE</code>: detalle de una PC\n"+
			"• <code>/pantalla NOMBRE</code>: captura de su pantalla", nil)
	}
}

func (b *telegramBot) sendStatus(chatID int64, onlyOnline bool) {
	text, markup := b.statusText(onlyOnline)
	b.send(chatID, text, markup)
}

// handleButton atiende los botones bajo el estado: "s:<pc>" stats, "p:<pc>" pantalla.
func (b *telegramBot) handleButton(id string, chatID int64, username, data string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
	cancel()
	if !strings.EqualFold(username, b.cfg.User) {
		return
	}
	kind, name, _ := strings.Cut(data, ":")
	switch kind {
	case "s":
		b.send(chatID, b.machineText(name), nil)
	case "p":
		b.screen(chatID, name)
	}
}

// screen manda una captura de la pantalla de una PC.
func (b *telegramBot) screen(chatID int64, name string) {
	b.p.reload()
	name = b.resolve(name)
	img, mime, err := b.p.capture(context.Background(), name)
	if err != nil {
		b.send(chatID, fmt.Sprintf("🖥 <b>%s</b>: no pude capturar la pantalla — %s", esc(name), esc(err.Error())), nil)
		return
	}
	caption := fmt.Sprintf("🖥 <b>%s</b> · %s", esc(name), time.Now().Format("15:04:05"))
	if err := b.sendPhoto(chatID, img, mime, caption); err != nil {
		b.logger.Printf("telegram: %v", err)
		b.send(chatID, "No pude enviar la captura: "+esc(err.Error()), nil)
	}
}

// resolve acepta el nombre sin distinguir mayúsculas (a mano se escribe "desktop-…").
func (b *telegramBot) resolve(name string) string {
	b.p.mu.Lock()
	defer b.p.mu.Unlock()
	for _, m := range b.p.machines {
		if strings.EqualFold(m.Name, name) {
			return m.Name
		}
	}
	return name
}

// rememberChat guarda el chat_id en telegram.json la primera vez.
func (b *telegramBot) rememberChat(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.ChatID == chatID {
		return
	}
	b.cfg.ChatID = chatID
	data, _ := json.MarshalIndent(b.cfg, "", "  ")
	if err := os.WriteFile(b.path, append(data, '\n'), 0o600); err != nil {
		b.logger.Printf("telegram: guardar chat_id: %v", err)
		return
	}
	b.logger.Printf("telegram: chat de @%s registrado", b.cfg.User)
}

// statusText consulta toda la flota ahora mismo y la resume; devuelve además
// los botones de stats/pantalla de cada PC encendida.
func (b *telegramBot) statusText(onlyOnline bool) (string, any) {
	machines := b.p.reload()
	if len(machines) == 0 {
		return "No hay PCs configuradas en Astrea.", nil
	}
	out := make([]machineStatus, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func(i int, m Machine) {
			defer wg.Done()
			out[i] = b.p.poll(context.Background(), m)
		}(i, m)
	}
	wg.Wait()

	on := 0
	var rows [][]map[string]string
	for _, s := range out {
		if s.Online {
			on++
			rows = append(rows, []map[string]string{
				{"text": "📈 " + s.Name, "callback_data": trimData("s:" + s.Name)},
				{"text": "🖥 Pantalla", "callback_data": trimData("p:" + s.Name)},
			})
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Astrea: %d/%d activas</b>\n", on, len(out))
	for _, s := range out {
		if !s.Online {
			if !onlyOnline {
				fmt.Fprintf(&sb, "\n%s · <b>%s</b>", badge(s), esc(s.Name))
			}
			continue
		}
		fmt.Fprintf(&sb, "\n%s · <b>%s</b>%s", badge(s), esc(s.Name), statsLine(s))
	}
	if onlyOnline && on == 0 {
		sb.WriteString("\nNinguna encendida ahora mismo.")
	}
	if len(rows) == 0 {
		return sb.String(), nil
	}
	return sb.String(), map[string]any{"inline_keyboard": rows}
}

// trimData respeta el límite de 64 bytes de callback_data de Telegram.
func trimData(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// machineText es el detalle de una PC: sistema, CPU, RAM, GPUs, discos, uso y servicios.
func (b *telegramBot) machineText(name string) string {
	b.p.reload()
	name = b.resolve(name)
	var m *Machine
	b.p.mu.Lock()
	for i := range b.p.machines {
		if b.p.machines[i].Name == name {
			m = &b.p.machines[i]
		}
	}
	b.p.mu.Unlock()
	if m == nil {
		return "No conozco ninguna PC llamada <b>" + esc(name) + "</b>."
	}
	s := b.p.poll(context.Background(), *m)
	if !s.Online {
		return fmt.Sprintf("🔴 Sin conexión · <b>%s</b> está apagada o fuera del tailnet.", esc(name))
	}

	var st struct {
		Host struct {
			Platform        string `json:"platform"`
			PlatformVersion string `json:"platform_version"`
			UptimeSec       uint64 `json:"uptime_sec"`
		} `json:"host"`
		CPU struct {
			Model        string  `json:"model"`
			Cores        int     `json:"cores"`
			Threads      int     `json:"threads"`
			UsagePercent float64 `json:"usage_percent"`
		} `json:"cpu"`
		Memory struct {
			Total       uint64  `json:"total"`
			Used        uint64  `json:"used"`
			UsedPercent float64 `json:"used_percent"`
		} `json:"memory"`
		GPUs []struct {
			Name         string   `json:"name"`
			VRAMTotal    uint64   `json:"vram_total"`
			VRAMUsed     *uint64  `json:"vram_used"`
			UsagePercent *float64 `json:"usage_percent"`
			TemperatureC *float64 `json:"temperature_c"`
			SharedMemory bool     `json:"shared_memory"`
		} `json:"gpus"`
		Disks []struct {
			Mount       string  `json:"mount"`
			Total       uint64  `json:"total"`
			Used        uint64  `json:"used"`
			UsedPercent float64 `json:"used_percent"`
		} `json:"disks"`
	}
	var idle struct {
		IdleSec    float64 `json:"idle_sec"`
		Foreground string  `json:"foreground"`
		Process    string  `json:"process"`
		UserActive bool    `json:"user_active"`
	}
	var admin struct {
		Mode   string `json:"mode"`
		Locked bool   `json:"locked"`
	}
	var svc struct {
		Services []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	}
	_ = json.Unmarshal(s.Admin, &admin)

	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b> · %s", esc(name), badge(s))
	if admin.Mode != "" {
		fmt.Fprintf(&sb, " · modo %s", esc(admin.Mode))
	}
	if s.Stats != nil && json.Unmarshal(s.Stats, &st) == nil {
		fmt.Fprintf(&sb, "\n%s %s · encendida hace %s",
			esc(st.Host.Platform), esc(st.Host.PlatformVersion), ago(time.Duration(st.Host.UptimeSec)*time.Second))
		fmt.Fprintf(&sb, "\n\n<b>CPU</b> %.0f%% — %s (%dc/%dt)", st.CPU.UsagePercent, esc(st.CPU.Model), st.CPU.Cores, st.CPU.Threads)
		fmt.Fprintf(&sb, "\n<b>RAM</b> %.0f%% — %s / %s", st.Memory.UsedPercent, gb(st.Memory.Used), gb(st.Memory.Total))
		for _, g := range st.GPUs {
			fmt.Fprintf(&sb, "\n<b>GPU</b> %s", esc(g.Name))
			if g.UsagePercent != nil {
				fmt.Fprintf(&sb, " · %.0f%%", *g.UsagePercent)
			}
			if g.VRAMUsed != nil {
				label := "VRAM"
				if g.SharedMemory {
					label = "compartida"
				}
				fmt.Fprintf(&sb, " · %s %s / %s", label, gb(*g.VRAMUsed), gb(g.VRAMTotal))
			}
			if g.TemperatureC != nil {
				fmt.Fprintf(&sb, " · %.0f°C", *g.TemperatureC)
			}
		}
		for _, d := range st.Disks {
			fmt.Fprintf(&sb, "\n<b>Disco</b> %s %.0f%% — %s / %s", esc(d.Mount), d.UsedPercent, gb(d.Used), gb(d.Total))
		}
	}
	if s.Idle != nil && json.Unmarshal(s.Idle, &idle) == nil {
		if idle.UserActive {
			fmt.Fprintf(&sb, "\n\n🟡 <b>En uso</b>")
			if idle.Foreground != "" || idle.Process != "" {
				fmt.Fprintf(&sb, " — %s (%s)", esc(idle.Foreground), esc(idle.Process))
			}
		} else {
			fmt.Fprintf(&sb, "\n\n🟢 Libre (sin actividad hace %s)", ago(time.Duration(idle.IdleSec)*time.Second))
		}
	}
	if s.Services != nil && json.Unmarshal(s.Services, &svc) == nil && len(svc.Services) > 0 {
		sb.WriteString("\n\n<b>Servicios</b>")
		for _, x := range svc.Services {
			fmt.Fprintf(&sb, "\n• %s — %s", esc(x.Name), esc(x.State))
		}
	}
	return sb.String()
}

func gb(n uint64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

// badge es el estado de una PC con el mismo vocabulario y emojis que el
// panel web: 🟢 Libre · 🟡 En uso · 🔴 Sin conexión · 🔒 Bloqueada.
func badge(s machineStatus) string {
	if !s.Online {
		return "🔴 Sin conexión"
	}
	var admin struct {
		Locked bool `json:"locked"`
	}
	if json.Unmarshal(s.Admin, &admin) == nil && admin.Locked {
		return "🔒 Bloqueada"
	}
	var idle struct {
		UserActive bool `json:"user_active"`
	}
	if s.Idle != nil && json.Unmarshal(s.Idle, &idle) == nil && idle.UserActive {
		return "🟡 En uso"
	}
	return "🟢 Libre"
}

// statsLine resume CPU/RAM y si alguien está usando la PC.
func statsLine(s machineStatus) string {
	var st struct {
		CPU struct {
			UsagePercent float64 `json:"usage_percent"`
		} `json:"cpu"`
		Memory struct {
			UsedPercent float64 `json:"used_percent"`
		} `json:"memory"`
	}
	var idle struct {
		IdleSec    float64 `json:"idle_sec"`
		Process    string  `json:"process"`
		UserActive bool    `json:"user_active"`
	}
	var parts []string
	if json.Unmarshal(s.Stats, &st) == nil && s.Stats != nil {
		parts = append(parts, fmt.Sprintf("CPU %.0f%% · RAM %.0f%%", st.CPU.UsagePercent, st.Memory.UsedPercent))
	}
	if json.Unmarshal(s.Idle, &idle) == nil && s.Idle != nil {
		if idle.UserActive && idle.Process != "" {
			parts = append(parts, idle.Process)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n    " + esc(strings.Join(parts, " · "))
}

// --- vigilancia ---

// watch sondea la flota y avisa de cada cambio encendida↔apagada. La primera
// ronda solo fija la línea base y manda un resumen.
func (b *telegramBot) watch() {
	state := map[string]*machineWatch{}
	first := true
	tick := time.NewTicker(time.Duration(b.cfg.Interval) * time.Second)
	defer tick.Stop()
	for ; ; <-tick.C {
		machines := b.p.reload()
		alive := make([]bool, len(machines))
		var wg sync.WaitGroup
		for i, m := range machines {
			wg.Add(1)
			go func(i int, m Machine) {
				defer wg.Done()
				alive[i] = b.alive(m)
			}(i, m)
		}
		wg.Wait()

		now := time.Now()
		seen := map[string]bool{}
		var events []string
		for i, m := range machines {
			seen[m.Name] = true
			w := state[m.Name]
			if w == nil {
				state[m.Name] = &machineWatch{online: alive[i], since: now}
				continue // primera vez que la vemos: sin aviso
			}
			switch {
			case alive[i]:
				w.fails = 0
				if !w.online {
					events = append(events, fmt.Sprintf("🟢 <b>%s</b> se encendió (estuvo apagada %s)", esc(m.Name), ago(now.Sub(w.since))))
					w.online, w.since = true, now
				}
			case w.online:
				// Dos fallos seguidos antes de darla por apagada.
				if w.fails++; w.fails >= 2 {
					events = append(events, fmt.Sprintf("🔴 <b>%s</b> se apagó o perdió conexión (estuvo encendida %s)", esc(m.Name), ago(now.Sub(w.since))))
					w.online, w.since = false, now
				}
			}
		}
		for name := range state {
			if !seen[name] {
				delete(state, name)
			}
		}

		if first {
			first = false
			if len(machines) > 0 {
				b.notify("🛰 Astrea vigilando la flota.\n\n" + b.summary(state))
			}
			continue
		}
		if len(events) > 0 {
			b.notify(strings.Join(events, "\n") + "\n\n" + b.summary(state))
		}
	}
}

func (b *telegramBot) alive(m Machine) bool {
	b.p.mu.Lock()
	client := b.p.clients[m.WS]
	b.p.mu.Unlock()
	if client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "admin.status", map[string]any{})
	return err == nil
}

// summary: "Activas 2/3: pc-a, pc-b".
func (b *telegramBot) summary(state map[string]*machineWatch) string {
	var on []string
	for name, w := range state {
		if w.online {
			on = append(on, name)
		}
	}
	sort.Strings(on)
	s := fmt.Sprintf("Activas %d/%d", len(on), len(state))
	if len(on) > 0 {
		s += ": " + esc(strings.Join(on, ", "))
	}
	return s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dmin", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d días", int(d.Hours()/24))
	}
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
