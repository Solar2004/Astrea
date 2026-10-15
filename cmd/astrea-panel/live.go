// Ventana en vivo: la pantalla de un equipo como vídeo MJPEG (un <img> en el
// navegador) y control con ratón y teclado desde el panel.
//
//	GET  /api/live?machine=&fps=&w=&q=   multipart/x-mixed-replace de JPEG
//	POST /api/control {machine, on}      tomar / soltar el control (latido cada ≤10 s)
//	POST /api/input   {machine, type, …} una acción de ratón o teclado
//
// Mientras el humano tiene el control, el agente rechaza el ratón y teclado
// de la IA (panel.control); las acciones del panel van marcadas "from":"panel".
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// screenGeo es la geometría de la última captura de cada equipo, para pasar
// las coordenadas relativas del navegador (0..1) a píxeles de pantalla.
type screenGeo struct {
	X, Y, W, H int
}

var geos sync.Map // nombre → screenGeo

type liveFrame struct {
	ScreenWidth  int `json:"screen_width"`
	ScreenHeight int `json:"screen_height"`
	Origin       struct {
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"origin"`
	Images []struct {
		Data string `json:"data"`
	} `json:"images"`
}

func (p *panel) frame(ctx context.Context, machine string, width, quality int) ([]byte, error) {
	var f liveFrame
	if _, err := p.call(ctx, machine, "screen.capture", map[string]any{"max_width": width, "quality": quality, "format": "jpeg"}, &f, 15*time.Second); err != nil {
		return nil, err
	}
	if len(f.Images) == 0 {
		return nil, errors.New("sin imagen (¿nadie ha iniciado sesión?)")
	}
	geos.Store(machine, screenGeo{f.Origin.X, f.Origin.Y, f.ScreenWidth, f.ScreenHeight})
	return base64.StdEncoding.DecodeString(f.Images[0].Data)
}

func queryInt(r *http.Request, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		v = def
	}
	return min(max(v, lo), hi)
}

func (p *panel) handleLive(w http.ResponseWriter, r *http.Request) {
	machine := r.URL.Query().Get("machine")
	if p.client(machine) == nil {
		http.Error(w, errUnknownMachine.Error(), http.StatusNotFound)
		return
	}
	fps := queryInt(r, "fps", 8, 1, 15)
	width := queryInt(r, "w", 1280, 320, 2560)
	quality := queryInt(r, "q", 60, 20, 90)
	first, err := p.frame(r.Context(), machine, width, quality)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	const boundary = "astreaframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	period := time.Second / time.Duration(fps)
	img := first
	for {
		start := time.Now()
		if _, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(img)); err != nil {
			return
		}
		if _, err := w.Write(append(img, '\r', '\n')); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if d := period - time.Since(start); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		next, err := p.frame(r.Context(), machine, width, quality)
		if r.Context().Err() != nil {
			return
		}
		if err == nil {
			img = next
		} else {
			time.Sleep(time.Second) // un fallo suelto: se reintenta con la última imagen
		}
	}
}

// handleControl: POST {machine, on}. El navegador repite on cada ≤10 s
// mientras tenga el control; si deja de hacerlo, caduca solo a los 30 s.
func (p *panel) handleControl(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Machine string `json:"machine"`
		On      bool   `json:"on"`
		Renew   bool   `json:"renew"` // latido: no registra aviso
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if code, err := p.call(r.Context(), req.Machine, "panel.control", map[string]any{"on": req.On, "ttl_sec": 30}, nil, 10*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	if !req.Renew {
		if req.On {
			p.events.add(event{Machine: req.Machine, Kind: "control.on", Level: "warn", button: "m",
				html: fmt.Sprintf("🕹 <b>%s</b>: tomaste el control desde el panel; la IA no puede usar ratón ni teclado", esc(req.Machine))})
		} else {
			p.events.add(event{Machine: req.Machine, Kind: "control.off", Level: "info", button: "m",
				html: fmt.Sprintf("🕹 <b>%s</b>: soltaste el control; la IA vuelve a tener ratón y teclado", esc(req.Machine))})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"on": req.On})
}

// inputReq es una acción del navegador. fx, fy: posición relativa (0..1)
// sobre la imagen en vivo.
type inputReq struct {
	Machine string   `json:"machine"`
	Type    string   `json:"type"` // move, click, down, up, scroll, key, text
	FX      *float64 `json:"fx,omitempty"`
	FY      *float64 `json:"fy,omitempty"`
	Button  string   `json:"button,omitempty"`
	Double  bool     `json:"double,omitempty"`
	DX      int      `json:"dx,omitempty"`
	DY      int      `json:"dy,omitempty"`
	Keys    string   `json:"keys,omitempty"`
	Text    string   `json:"text,omitempty"`
}

func (p *panel) handleInput(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	var req inputReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	do := func(method string, args map[string]any) error {
		args["from"] = "panel"
		code, err := p.call(ctx, req.Machine, method, args, nil, 10*time.Second)
		if err != nil {
			fail(w, code, err)
		}
		return err
	}
	// Las acciones con posición mueven primero el ratón ahí.
	if req.FX != nil && req.FY != nil {
		g, ok := geos.Load(req.Machine)
		if !ok {
			fail(w, http.StatusConflict, errors.New("abre la vista en vivo antes de controlar"))
			return
		}
		geo := g.(screenGeo)
		fx, fy := min(max(*req.FX, 0), 1), min(max(*req.FY, 0), 1)
		x := geo.X + int(math.Round(fx*float64(geo.W-1)))
		y := geo.Y + int(math.Round(fy*float64(geo.H-1)))
		if do("mouse.move", map[string]any{"x": x, "y": y}) != nil {
			return
		}
	}
	var err error
	switch req.Type {
	case "move":
	case "click":
		err = do("mouse.click", map[string]any{"button": req.Button, "double": req.Double})
	case "down", "up":
		err = do("mouse.button", map[string]any{"button": req.Button, "action": req.Type})
	case "scroll":
		err = do("mouse.scroll", map[string]any{"dx": req.DX, "dy": req.DY})
	case "key":
		err = do("key.press", map[string]any{"keys": req.Keys})
	case "text":
		err = do("key.type", map[string]any{"text": req.Text})
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("type desconocido %q", req.Type))
		return
	}
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
