// Bot de Telegram: el panel de bolsillo. Tarjetas PNG (tgcard.go) con el
// estado de la flota y de cada equipo, botones que editan el mismo mensaje
// (una sola "pantalla" que cambia), freno de emergencia, servicios, actividad
// de la IA y avisos proactivos (encendido/apagado, temperatura, disco, RAM,
// servicios caídos, freno). Especificación en docs/DESIGN.md › Bot de Telegram.
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	btnFleet     = "📊 Flota"
	btnOnline    = "🟢 Activas"
	btnOldStatus = "📊 Estado" // teclado de versiones anteriores
)

type telegramConfig struct {
	Token    string `json:"token"`
	User     string `json:"user"`                   // username autorizado, sin @
	ChatID   int64  `json:"chat_id,omitempty"`      // se aprende con /start
	Interval int    `json:"interval_sec,omitempty"` // cada cuánto se vigila (def. 20)
	// Alerts silencia (false) los avisos; los del freno se mandan siempre.
	Alerts *bool `json:"alerts,omitempty"`
}

type telegramBot struct {
	p      *panel
	path   string
	logger *log.Logger
	client *http.Client

	mu  sync.Mutex
	cfg telegramConfig
	// botLock: cuándo cambió el bot el freno de cada equipo, para no avisar
	// de un cambio que el usuario acaba de hacer él mismo.
	botLock map[string]time.Time

	kmu    sync.Mutex
	keys   map[string]tgAction // callback_data largo → id corto ("k:<id>")
	keySeq int
}

// tgAction es una acción con argumentos que no caben en los 64 bytes de
// callback_data (nombre de equipo + nombre de servicio).
type tgAction struct {
	Kind, Machine, Service string
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
	b := &telegramBot{p: p, path: path, logger: logger, cfg: cfg,
		client:  &http.Client{Timeout: 70 * time.Second},
		botLock: map[string]time.Time{}, keys: map[string]tgAction{}}
	if cfg.ChatID == 0 {
		logger.Printf("telegram: activo; @%s tiene que mandarle /start al bot para recibir avisos", cfg.User)
	} else {
		logger.Printf("telegram: activo, avisando a @%s", cfg.User)
	}
	go b.setCommands()
	go b.listen()
	go b.watch()
}

func telegramConfigPath(machinesPath string) string {
	return filepath.Join(filepath.Dir(machinesPath), "telegram.json")
}

// --- API de Telegram ---

// telegramAPI es la base de la Bot API; ASTREA_TELEGRAM_API la cambia (pruebas).
func telegramAPI() string {
	if u := os.Getenv("ASTREA_TELEGRAM_API"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.telegram.org"
}

// cleanErr quita la URL (que lleva el token) de los errores de red.
func cleanErr(method string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%s: %w", method, err)
}

func (b *telegramBot) call(ctx context.Context, method string, req, out any) error {
	body, _ := json.Marshal(req)
	u := telegramAPI() + "/bot" + b.cfg.Token + "/" + method
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(hreq)
	if err != nil {
		return cleanErr(method, err)
	}
	defer resp.Body.Close()
	return decodeTG(method, resp.Body, out)
}

func decodeTG(method string, body io.Reader, out any) error {
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(body).Decode(&r); err != nil {
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

// upload llama a un método con un archivo adjunto (multipart).
func (b *telegramBot) upload(method string, fields map[string]string, field, filename string, data []byte) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile(field, filename)
	_, _ = fw.Write(data)
	_ = mw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, telegramAPI()+"/bot"+b.cfg.Token+"/"+method, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.client.Do(req)
	if err != nil {
		return cleanErr(method, err)
	}
	defer resp.Body.Close()
	return decodeTG(method, resp.Body, nil)
}

// notModified: Telegram rechaza editar un mensaje con el mismo contenido.
func notModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// keyboard es el teclado fijo de abajo.
func keyboard() map[string]any {
	return map[string]any{
		"keyboard":        [][]map[string]string{{{"text": btnFleet}, {"text": btnOnline}}},
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
		"chat_id": chatID, "text": text, "parse_mode": "HTML", "reply_markup": markup,
	}, nil)
	if err != nil {
		b.logger.Printf("telegram: %v", err)
	}
}

// notify manda un aviso al usuario, si ya se presentó con /start.
func (b *telegramBot) notify(text string, markup any) {
	b.mu.Lock()
	chatID := b.cfg.ChatID
	b.mu.Unlock()
	if chatID != 0 {
		b.send(chatID, text, markup)
	}
}

func (b *telegramBot) setCommands() {
	cmds := []map[string]string{
		{"command": "flota", "description": "Estado de todos los equipos"},
		{"command": "equipo", "description": "Detalle de un equipo: /equipo NOMBRE"},
		{"command": "pantalla", "description": "Ver la pantalla: /pantalla NOMBRE"},
		{"command": "actividad", "description": "Lo último que hizo la IA: /actividad NOMBRE"},
		{"command": "frenar", "description": "Freno de emergencia de la IA: /frenar NOMBRE"},
		{"command": "liberar", "description": "Quitar el freno: /liberar NOMBRE"},
		{"command": "avisos", "description": "Activar o silenciar los avisos"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil); err != nil {
		b.logger.Printf("telegram: %v", err)
	}
}

// --- vistas ---

// view es lo que ocupa "la pantalla" del bot: una imagen, su pie y botones.
type view struct {
	img      []byte
	filename string
	caption  string
	markup   any
}

func btn(text, data string) map[string]string {
	return map[string]string{"text": text, "callback_data": data}
}

func inline(rows ...[]map[string]string) map[string]any {
	return map[string]any{"inline_keyboard": rows}
}

// show pone v en el mensaje msgID (editándolo) o, si es 0 o no se puede
// editar (mensaje de texto, muy antiguo…), en un mensaje nuevo.
func (b *telegramBot) show(chatID int64, msgID int, v view) {
	if msgID != 0 {
		media := map[string]any{"type": "photo", "media": "attach://img", "caption": v.caption, "parse_mode": "HTML"}
		err := b.upload("editMessageMedia", map[string]string{
			"chat_id": fmt.Sprint(chatID), "message_id": fmt.Sprint(msgID),
			"media": jsonString(media), "reply_markup": jsonString(v.markup),
		}, "img", v.filename, v.img)
		if err == nil || notModified(err) {
			return
		}
	}
	err := b.upload("sendPhoto", map[string]string{
		"chat_id": fmt.Sprint(chatID), "caption": v.caption, "parse_mode": "HTML",
		"reply_markup": jsonString(v.markup),
	}, "photo", v.filename, v.img)
	if err != nil {
		b.logger.Printf("telegram: %v", err)
		b.send(chatID, "No pude enviar la imagen: "+esc(err.Error()), nil)
	}
}

// showCaption cambia solo el pie y los botones de la tarjeta (listas de
// servicios, actividad, confirmaciones). Si el mensaje no es una foto, manda
// la tarjeta del equipo con ese pie.
func (b *telegramBot) showCaption(chatID int64, msgID int, machine, caption string, markup any) {
	if msgID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := b.call(ctx, "editMessageCaption", map[string]any{
			"chat_id": chatID, "message_id": msgID, "caption": caption, "parse_mode": "HTML", "reply_markup": markup,
		}, nil)
		cancel()
		if err == nil || notModified(err) {
			return
		}
	}
	v, err := b.machineView(machine)
	if err != nil {
		b.send(chatID, caption, markup)
		return
	}
	v.caption, v.markup = caption, markup
	b.show(chatID, 0, v)
}

// emoji del estado, mismo vocabulario que el panel.
func stateEmoji(label string) string {
	switch label {
	case "En uso":
		return "🟡"
	case "Sin conexión":
		return "🔴"
	case "Bloqueada":
		return "🔒"
	}
	return "🟢"
}

func (b *telegramBot) pollAll() []machineStatus {
	machines := b.p.reload()
	out := make([]machineStatus, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = b.p.poll(context.Background(), m)
		}()
	}
	wg.Wait()
	return out
}

func (b *telegramBot) fleetView(onlyOnline bool) (view, error) {
	all := b.pollAll()
	list := all
	if onlyOnline {
		list = nil
		for _, s := range all {
			if s.Online {
				list = append(list, s)
			}
		}
	}
	img, err := renderFleetCard(list)
	if err != nil {
		return view{}, err
	}
	on, free, busy := 0, 0, 0
	var rows [][]map[string]string
	var row []map[string]string
	for _, s := range list {
		c := cardFrom(s)
		if s.Online {
			on++
		}
		switch c.Label {
		case "Libre", "En línea":
			free++
		case "En uso":
			busy++
		}
		row = append(row, btn(stateEmoji(c.Label)+" "+s.Name, "m:"+s.Name))
		if len(row) == 2 {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	refresh := "f"
	if onlyOnline {
		refresh = "fo"
	}
	rows = append(rows, []map[string]string{btn("🔄 Actualizar", refresh)})
	onAll := 0
	for _, s := range all {
		if s.Online {
			onAll++
		}
	}
	caption := fmt.Sprintf("🛰 <b>%d de %d en línea</b> · %d %s · %d en uso", onAll, len(all), free, plural(free, "libre", "libres"), busy)
	if len(all) == 0 {
		caption = "🛰 <b>Aún no hay equipos.</b> Añádelos desde el panel: Añadir un equipo."
	} else if onlyOnline && on == 0 {
		caption = "🛰 <b>Ningún equipo en línea ahora mismo.</b>"
	}
	return view{img: img, filename: "flota.png", caption: caption, markup: inline(rows...)}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

var errNoMachine = errors.New("no conozco ese equipo")

func (b *telegramBot) status(name string) (machineStatus, error) {
	b.p.reload()
	m := b.p.machine(b.resolve(name))
	if m == nil {
		return machineStatus{}, errNoMachine
	}
	return b.p.poll(context.Background(), *m), nil
}

func (b *telegramBot) machineView(name string) (view, error) {
	s, err := b.status(name)
	if err != nil {
		return view{}, err
	}
	img, err := renderMachineCard(s, b.p.history(s.Name))
	if err != nil {
		return view{}, err
	}
	c := cardFrom(s)
	caption := fmt.Sprintf("%s <b>%s</b> · %s", stateEmoji(c.Label), esc(s.Name), c.Label)
	if c.Detail != "" {
		caption += " · " + esc(lowerFirst(c.Detail))
	}
	n := s.Name
	var rows [][]map[string]string
	switch {
	case !s.Online:
		caption += "\n🔴 No responde: ¿está encendido y con Tailscale?"
		rows = [][]map[string]string{{btn("🔄 Actualizar", "m:"+n)}}
	default:
		lock := btn("🔒 Frenar IA", "l:"+n)
		if c.Locked {
			caption += "\n🔒 La IA no puede hacer nada aquí hasta que la liberes."
			lock = btn("🔓 Liberar", "u:"+n)
		}
		var admin struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(s.Admin, &admin)
		switch admin.Mode {
		case "yolo", "":
			caption += "\n⚡ YOLO · la IA tiene control total"
		case "readonly":
			caption += "\n👁 Solo lectura · la IA puede mirar, no cambiar nada"
		}
		rows = [][]map[string]string{
			{btn("🖥 Pantalla", "p:"+n), btn("🔄 Actualizar", "m:"+n)},
			{btn("⚙️ Servicios", "s:"+n), btn("📜 Actividad", "a:"+n)},
			{lock},
		}
	}
	rows = append(rows, []map[string]string{btn("← Flota", "f")})
	return view{img: img, filename: n + ".png", caption: caption, markup: inline(rows...)}, nil
}

func (b *telegramBot) screenView(name string) (view, error) {
	name = b.resolve(name)
	img, mime, err := b.p.capture(context.Background(), name)
	if err != nil {
		return view{}, err
	}
	fn := "pantalla.jpg"
	if mime == "image/png" {
		fn = "pantalla.png"
	}
	return view{img: img, filename: fn,
		caption: fmt.Sprintf("🖥 <b>%s</b> · %s", esc(name), time.Now().Format("15:04:05")),
		markup:  inline([]map[string]string{btn("🔄 Otra", "p:"+name), btn("← Equipo", "m:"+name)}),
	}, nil
}

// --- servicios y actividad (en el pie de la tarjeta) ---

type tgService struct{ Name, State string }

func (b *telegramBot) services(name string) ([]tgService, error) {
	var res struct {
		Services []tgService `json:"services"`
	}
	if _, err := b.p.call(context.Background(), name, "service.status", map[string]any{}, &res, 10*time.Second); err != nil {
		return nil, err
	}
	return res.Services, nil
}

func serviceEmoji(state string) string {
	switch state {
	case "running":
		return "🟢"
	case "starting", "backoff":
		return "🟡"
	case "failed", "exited":
		return "🔴"
	}
	return "⚪"
}

func (b *telegramBot) servicesCaption(name string) (string, any) {
	name = b.resolve(name)
	back := []map[string]string{btn("← Equipo", "m:"+name)}
	list, err := b.services(name)
	if err != nil {
		return fmt.Sprintf("⚙️ <b>%s</b>: no pude leer los servicios — %s", esc(name), esc(err.Error())), inline(back)
	}
	if len(list) == 0 {
		return fmt.Sprintf("⚙️ <b>%s</b> no tiene servicios.\nLa IA los crea con service.start (llama-server, ComfyUI…).", esc(name)), inline(back)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚙️ <b>%s</b> · servicios\n", esc(name))
	var rows [][]map[string]string
	for _, s := range list {
		fmt.Fprintf(&sb, "\n%s <b>%s</b> — %s", serviceEmoji(s.State), esc(s.Name), esc(s.State))
		row := []map[string]string{btn("↻ "+s.Name, b.key(tgAction{"svc-restart", name, s.Name}))}
		if s.State == "running" || s.State == "starting" || s.State == "backoff" {
			row = append(row, btn("⏹", b.key(tgAction{"svc-stop-ask", name, s.Name})))
		} else {
			row = append(row, btn("▶", b.key(tgAction{"svc-start", name, s.Name})))
		}
		rows = append(rows, row)
	}
	return sb.String(), inline(append(rows, back)...)
}

// verbs: mismas frases que la pestaña Actividad del panel (web/index.html).
var verbs = map[string]string{
	"exec": "Ejecutó un comando", "fs.list": "Miró una carpeta", "fs.read": "Leyó un archivo", "fs.write": "Escribió un archivo",
	"proc.list": "Listó los procesos", "proc.kill": "Cerró un proceso", "screen.capture": "Capturó la pantalla",
	"mouse.move": "Movió el ratón", "mouse.click": "Hizo clic", "key.type": "Escribió con el teclado",
	"endpoint.create": "Creó una API", "endpoint.delete": "Borró una API", "endpoint.list": "Listó sus APIs",
	"service.start": "Inició un servicio", "service.stop": "Detuvo un servicio", "service.restart": "Reinició un servicio",
	"service.create": "Creó un servicio", "service.delete": "Borró un servicio", "service.logs": "Leyó el registro de un servicio",
	"deps.check": "Comprobó dependencias", "deps.ensure": "Instaló dependencias", "net.download": "Descargó un archivo",
	"job.start": "Lanzó una tarea", "job.cancel": "Canceló una tarea", "admin.lock": "Cambió el freno de emergencia",
	"admin.update": "Se actualizó", "sys.stats": "Consultó el rendimiento", "sys.idle": "Comprobó si hay alguien",
	"sys.info": "Consultó el sistema", "admin.status": "Consultó su estado", "service.status": "Consultó los servicios",
}

// autoMethods son las consultas que hacen el panel y el bot solos.
var autoMethods = map[string]bool{"sys.stats": true, "sys.idle": true, "sys.info": true, "admin.status": true,
	"admin.audit": true, "service.status": true, "capabilities": true, "ping": true}

func (b *telegramBot) activityCaption(name string) (string, any) {
	name = b.resolve(name)
	back := inline([]map[string]string{btn("🔄 Actualizar", "a:"+name), btn("← Equipo", "m:"+name)})
	var res struct {
		Entries []struct {
			Time   string `json:"time"`
			Method string `json:"method"`
			Args   string `json:"args"`
			OK     bool   `json:"ok"`
		} `json:"entries"`
	}
	if _, err := b.p.call(context.Background(), name, "admin.audit", map[string]any{"limit": 300}, &res, 10*time.Second); err != nil {
		return fmt.Sprintf("📜 <b>%s</b>: no pude leer la actividad — %s", esc(name), esc(err.Error())), back
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📜 <b>%s</b> · lo último que hizo la IA\n", esc(name))
	n := 0
	for _, e := range res.Entries {
		if autoMethods[e.Method] {
			continue
		}
		when := e.Time
		if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
			when = t.Local().Format("15:04")
			if t.Local().YearDay() != time.Now().YearDay() {
				when = t.Local().Format("02/01 15:04")
			}
		}
		v := verbs[e.Method]
		if v == "" {
			v = e.Method
		}
		mark := ""
		if !e.OK {
			mark = " ✗"
		}
		line := fmt.Sprintf("\n%s · %s%s", when, v, mark)
		if s := auditSummary(e.Method, e.Args); s != "" {
			if utf8.RuneCountInString(s) > 50 {
				s = string([]rune(s)[:50]) + "…"
			}
			line += " — <code>" + esc(s) + "</code>"
		}
		sb.WriteString(line)
		if n++; n == 10 {
			break
		}
	}
	if n == 0 {
		sb.WriteString("\nNada todavía: la IA no ha hecho nada en este equipo.")
	}
	return sb.String(), back
}

// --- acciones ---

func (b *telegramBot) key(a tgAction) string {
	b.kmu.Lock()
	defer b.kmu.Unlock()
	b.keySeq++
	id := strconv.Itoa(b.keySeq)
	b.keys[id] = a
	if len(b.keys) > 1000 { // los botones muy viejos caducan
		for k := range b.keys {
			if n, _ := strconv.Atoi(k); n < b.keySeq-800 {
				delete(b.keys, k)
			}
		}
	}
	return "k:" + id
}

func (b *telegramBot) action(id string) (tgAction, bool) {
	b.kmu.Lock()
	defer b.kmu.Unlock()
	a, ok := b.keys[id]
	return a, ok
}

func (b *telegramBot) setLock(name string, locked bool) error {
	b.mu.Lock()
	b.botLock[name] = time.Now()
	b.mu.Unlock()
	_, err := b.p.call(context.Background(), name, "admin.lock", map[string]any{"locked": locked}, nil, 10*time.Second)
	return err
}

// --- comandos ---

// listen recibe mensajes por long polling.
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
					MessageID int `json:"message_id"`
					Chat      struct {
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
				go b.handleButton(c.ID, c.Message.Chat.ID, c.Message.MessageID, c.From.Username, c.Data)
				continue
			}
			if u.Message == nil || u.Message.Chat.Type != "private" {
				continue
			}
			go b.handle(u.Message.Chat.ID, u.Message.From.Username, strings.TrimSpace(u.Message.Text))
		}
	}
}

func (b *telegramBot) authorized(username string) bool {
	return strings.EqualFold(username, b.cfg.User)
}

func (b *telegramBot) handle(chatID int64, username, text string) {
	if !b.authorized(username) {
		b.send(chatID, "Este bot es privado.", nil)
		return
	}
	b.rememberChat(chatID)
	fields := strings.Fields(text)
	cmd, arg := "", ""
	if len(fields) > 0 {
		cmd = strings.ToLower(strings.SplitN(fields[0], "@", 2)[0]) // /flota@bot → /flota
		arg = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	}
	switch {
	case text == btnFleet || text == btnOldStatus || cmd == "/flota" || cmd == "/estado" || cmd == "/status":
		b.render(chatID, 0, func() (view, error) { return b.fleetView(false) })
	case text == btnOnline || cmd == "/activas":
		b.render(chatID, 0, func() (view, error) { return b.fleetView(true) })
	case cmd == "/equipo" || cmd == "/stats":
		b.withMachine(chatID, arg, "m")
	case cmd == "/pantalla":
		b.withMachine(chatID, arg, "p")
	case cmd == "/actividad":
		b.withMachine(chatID, arg, "a")
	case cmd == "/frenar":
		b.withMachine(chatID, arg, "l")
	case cmd == "/liberar":
		b.withMachine(chatID, arg, "u")
	case cmd == "/avisos":
		b.toggleAlerts(chatID)
	default:
		b.send(chatID, "Hola 👋 Soy el panel de Astrea en tu bolsillo.\n\n"+
			"• <b>📊 Flota</b>: cómo están todos los equipos; toca uno para ver su detalle\n"+
			"• <code>/pantalla NOMBRE</code>: ver su pantalla\n"+
			"• <code>/actividad NOMBRE</code>: lo último que hizo la IA\n"+
			"• <code>/frenar NOMBRE</code>: freno de emergencia de la IA\n"+
			"• <code>/avisos</code>: silenciar o activar los avisos\n\n"+
			"Te aviso aquí si un equipo se apaga, se calienta o se queda sin disco.", nil)
	}
}

// withMachine ejecuta la acción kind sobre un equipo; sin nombre, usa el único
// que hay o pregunta con botones.
func (b *telegramBot) withMachine(chatID int64, arg, kind string) {
	if arg == "" {
		machines := b.p.reload()
		switch len(machines) {
		case 0:
			b.send(chatID, "Aún no hay equipos. Añádelos desde el panel.", nil)
			return
		case 1:
			arg = machines[0].Name
		default:
			var rows [][]map[string]string
			var row []map[string]string
			for _, m := range machines {
				row = append(row, btn(m.Name, kind+":"+m.Name))
				if len(row) == 2 {
					rows, row = append(rows, row), nil
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			b.send(chatID, "¿Qué equipo?", inline(rows...))
			return
		}
	}
	b.dispatch(chatID, 0, kind, b.resolve(arg))
}

// render genera una vista y la muestra; si falla, lo dice.
func (b *telegramBot) render(chatID int64, msgID int, fn func() (view, error)) {
	v, err := fn()
	if err != nil {
		b.send(chatID, "⚠️ "+esc(err.Error()), nil)
		return
	}
	b.show(chatID, msgID, v)
}

// dispatch atiende una acción "corta" (callback_data "kind:nombre").
func (b *telegramBot) dispatch(chatID int64, msgID int, kind, name string) {
	switch kind {
	case "m":
		b.render(chatID, msgID, func() (view, error) { return b.machineView(name) })
	case "p":
		v, err := b.screenView(name)
		if err != nil {
			b.send(chatID, fmt.Sprintf("🖥 <b>%s</b>: no pude capturar la pantalla — %s", esc(name), esc(err.Error())), nil)
			return
		}
		b.show(chatID, msgID, v)
	case "s":
		caption, markup := b.servicesCaption(name)
		b.showCaption(chatID, msgID, name, caption, markup)
	case "a":
		caption, markup := b.activityCaption(name)
		b.showCaption(chatID, msgID, name, caption, markup)
	case "l":
		b.showCaption(chatID, msgID, name,
			fmt.Sprintf("🔒 ¿Frenar la IA en <b>%s</b>?\nNo podrá hacer nada en este equipo hasta que lo liberes.", esc(name)),
			inline([]map[string]string{btn("Sí, frenar", "L:"+name), btn("Cancelar", "m:"+name)}))
	case "u":
		b.showCaption(chatID, msgID, name,
			fmt.Sprintf("🔓 ¿Liberar la IA en <b>%s</b>?\nVolverá a tener control total del equipo.", esc(name)),
			inline([]map[string]string{btn("Sí, liberar", "U:"+name), btn("Cancelar", "m:"+name)}))
	case "L", "U":
		if err := b.setLock(name, kind == "L"); err != nil {
			b.send(chatID, fmt.Sprintf("No pude cambiar el freno de <b>%s</b>: %s", esc(name), esc(err.Error())), nil)
			return
		}
		b.render(chatID, msgID, func() (view, error) { return b.machineView(name) })
	}
}

// handleButton atiende los botones inline.
func (b *telegramBot) handleButton(id string, chatID int64, msgID int, username, data string) {
	answer := func(text string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
		cancel()
	}
	if !b.authorized(username) {
		answer("Este bot es privado.")
		return
	}
	kind, arg, _ := strings.Cut(data, ":")
	switch kind {
	case "f", "fo":
		answer("Cargando…")
		b.render(chatID, msgID, func() (view, error) { return b.fleetView(kind == "fo") })
	case "k":
		a, ok := b.action(arg)
		if !ok {
			answer("Este botón ha caducado; vuelve a abrir los servicios.")
			return
		}
		b.serviceAction(chatID, msgID, a, answer)
	default:
		answer("Cargando…")
		b.dispatch(chatID, msgID, kind, arg)
	}
}

func (b *telegramBot) serviceAction(chatID int64, msgID int, a tgAction, answer func(string)) {
	switch a.Kind {
	case "svc-stop-ask":
		answer("")
		b.showCaption(chatID, msgID, a.Machine,
			fmt.Sprintf("⏹ ¿Detener <b>%s</b> en <b>%s</b>?\nSe queda parado hasta que lo vuelvas a iniciar.", esc(a.Service), esc(a.Machine)),
			inline([]map[string]string{btn("Sí, detener", b.key(tgAction{"svc-stop", a.Machine, a.Service})), btn("Cancelar", "s:"+a.Machine)}))
		return
	case "svc-restart", "svc-start", "svc-stop":
		answer("Un momento…")
		method, done := "service.start", "iniciado"
		if a.Kind == "svc-restart" {
			done = "reiniciado"
		}
		args := map[string]any{"name": a.Service, "wait_ready_sec": 20}
		if a.Kind == "svc-stop" {
			method, done, args = "service.stop", "detenido", map[string]any{"name": a.Service}
		}
		if _, err := b.p.call(context.Background(), a.Machine, method, args, nil, 40*time.Second); err != nil {
			b.send(chatID, fmt.Sprintf("⚙️ <b>%s</b> en %s: %s", esc(a.Service), esc(a.Machine), esc(err.Error())), nil)
		} else {
			b.logger.Printf("telegram: servicio %s %s en %s", a.Service, done, a.Machine)
		}
		caption, markup := b.servicesCaption(a.Machine)
		b.showCaption(chatID, msgID, a.Machine, caption, markup)
	}
}

func (b *telegramBot) alertsOn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.Alerts == nil || *b.cfg.Alerts
}

func (b *telegramBot) toggleAlerts(chatID int64) {
	b.mu.Lock()
	on := !(b.cfg.Alerts == nil || *b.cfg.Alerts)
	b.cfg.Alerts = &on
	b.saveLocked()
	b.mu.Unlock()
	if on {
		b.send(chatID, "🔔 Avisos <b>activados</b>: encendido/apagado, temperatura, disco, memoria y servicios caídos.", nil)
	} else {
		b.send(chatID, "🔕 Avisos <b>silenciados</b>. Los del freno de emergencia te llegan siempre. /avisos para volver a activarlos.", nil)
	}
}

// resolve acepta el nombre sin distinguir mayúsculas.
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
	b.saveLocked()
	b.logger.Printf("telegram: chat de @%s registrado", b.cfg.User)
}

// saveLocked escribe telegram.json; b.mu tiene que estar tomado.
func (b *telegramBot) saveLocked() {
	data, _ := json.MarshalIndent(b.cfg, "", "  ")
	if err := os.WriteFile(b.path, append(data, '\n'), 0o600); err != nil {
		b.logger.Printf("telegram: guardar %s: %v", b.path, err)
	}
}

// --- vigilancia y avisos ---

// machineWatch es lo que se sabe de un equipo entre rondas.
type machineWatch struct {
	online   bool
	fails    int       // fallos seguidos (para no avisar por un timeout suelto)
	since    time.Time // desde cuándo está en el estado actual
	locked   bool
	ramSince time.Time         // desde cuándo la RAM está ≥ 95 %
	services map[string]string // último estado de cada servicio
}

const alertCooldown = 30 * time.Minute

// watch sondea la flota y avisa de los cambios. La primera ronda solo fija la
// línea base y manda un resumen.
func (b *telegramBot) watch() {
	state := map[string]*machineWatch{}
	sent := map[string]time.Time{} // equipo|métrica → último aviso
	first := true
	tick := time.NewTicker(time.Duration(b.cfg.Interval) * time.Second)
	defer tick.Stop()
	for ; ; <-tick.C {
		list := b.pollAll()
		now := time.Now()
		seen := map[string]bool{}
		type alert struct {
			text, machine string
			always        bool
		}
		var alerts []alert
		threshold := func(name, metric, text string) {
			k := name + "|" + metric
			if now.Sub(sent[k]) < alertCooldown {
				return
			}
			sent[k] = now
			alerts = append(alerts, alert{text: text, machine: name})
		}

		for _, s := range list {
			name := s.Name
			seen[name] = true
			c := cardFrom(s)
			w := state[name]
			if w == nil {
				w = &machineWatch{online: s.Online, since: now, locked: c.Locked, services: map[string]string{}}
				state[name] = w
				b.fillServices(w, s, nil)
				continue // primera vez que lo vemos: sin aviso
			}
			// Encendido / apagado (dos fallos seguidos antes de darlo por apagado).
			switch {
			case s.Online:
				w.fails = 0
				if !w.online {
					alerts = append(alerts, alert{text: fmt.Sprintf("🟢 <b>%s</b> se encendió (estuvo apagado %s)", esc(name), ago(now.Sub(w.since))), machine: name})
					w.online, w.since = true, now
				}
			case w.online:
				if w.fails++; w.fails >= 2 {
					alerts = append(alerts, alert{text: fmt.Sprintf("🔴 <b>%s</b> se apagó o perdió conexión (estuvo encendido %s)", esc(name), ago(now.Sub(w.since))), machine: name})
					w.online, w.since = false, now
				}
			}
			if !s.Online {
				continue
			}
			// Freno: siempre se avisa, salvo si lo acaba de cambiar el propio bot.
			if c.Locked != w.locked {
				b.mu.Lock()
				byBot := now.Sub(b.botLock[name]) < 2*time.Minute
				b.mu.Unlock()
				if !byBot {
					t := fmt.Sprintf("🔒 <b>%s</b>: freno de emergencia <b>activado</b>. La IA no puede hacer nada ahí.", esc(name))
					if !c.Locked {
						t = fmt.Sprintf("🔓 <b>%s</b>: freno de emergencia <b>desactivado</b>. La IA vuelve a tener control.", esc(name))
					}
					alerts = append(alerts, alert{text: t, machine: name, always: true})
				}
				w.locked = c.Locked
			}
			// Umbrales.
			if c.Stats != nil {
				for _, g := range c.Stats.GPUs {
					if g.TempC != nil && *g.TempC >= 85 {
						threshold(name, "gpu-temp", fmt.Sprintf("🌡 <b>%s</b>: la gráfica está a %.0f °C", esc(name), *g.TempC))
					}
				}
				for _, d := range c.Stats.Disks {
					if d.UsedPct >= 90 {
						threshold(name, "disk|"+d.Mount, fmt.Sprintf("💾 <b>%s</b>: el disco %s está al %.0f %% (quedan %s)", esc(name), esc(d.Mount), d.UsedPct, cardBytes(d.Total-d.Used)))
					}
				}
				if val(c.Stats.Memory.UsedPct) >= 95 {
					if w.ramSince.IsZero() {
						w.ramSince = now
					} else if now.Sub(w.ramSince) >= 2*time.Minute {
						threshold(name, "ram", fmt.Sprintf("🧠 <b>%s</b>: la memoria lleva %s al %.0f %%", esc(name), ago(now.Sub(w.ramSince)), val(c.Stats.Memory.UsedPct)))
					}
				} else {
					w.ramSince = time.Time{}
				}
			}
			b.fillServices(w, s, func(svc string) {
				threshold(name, "svc|"+svc, fmt.Sprintf("⚙️ <b>%s</b>: el servicio <b>%s</b> ha fallado", esc(name), esc(svc)))
			})
		}
		for name := range state {
			if !seen[name] {
				delete(state, name)
			}
		}

		if first {
			first = false
			if len(list) > 0 && b.alertsOn() {
				b.notify("🛰 Astrea vigilando la flota.\n\n"+summary(list), nil)
			}
			continue
		}
		for _, a := range alerts {
			if a.always || b.alertsOn() {
				b.notify(a.text, inline([]map[string]string{btn("Ver equipo", "m:"+a.machine)}))
			}
		}
	}
}

// fillServices actualiza el estado de los servicios y llama a failed por cada
// uno que acaba de pasar a "failed".
func (b *telegramBot) fillServices(w *machineWatch, s machineStatus, failed func(string)) {
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

// summary: "🟢 f3-1 · Libre\n🔴 pc-aula · Sin conexión".
func summary(list []machineStatus) string {
	var sb strings.Builder
	for i, s := range list {
		if i > 0 {
			sb.WriteString("\n")
		}
		c := cardFrom(s)
		fmt.Fprintf(&sb, "%s <b>%s</b> · %s", stateEmoji(c.Label), esc(s.Name), c.Label)
	}
	return sb.String()
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
