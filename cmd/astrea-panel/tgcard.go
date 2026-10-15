package main

// PNG "cards" for the Telegram bot, drawn in pure Go with the panel's look
// (dark tokens of web/index.html, see docs/DESIGN.md). No browser, no CGo:
// golang.org/x/image/vector for shapes and opentype for text, fonts embedded.
//
//	renderFleetCard(list)       → the whole fleet: KPIs + one row per machine
//	renderMachineCard(s, hist)  → one machine: CPU/RAM/GPU with 2 h area charts, disks

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

//go:embed fonts/*.ttf
var cardFonts embed.FS

// ---------- tokens (dark theme of the panel) ----------

var (
	cBg       = rgb(0x0E0E12)
	cSurface  = rgb(0x18181D)
	cSurface2 = rgb(0x222229)
	cBorder   = rgb(0x26262D)
	cTrack    = rgb(0x2A2A32)
	cText     = rgb(0xF5F5F7)
	cText2    = rgb(0xB4B4BC)
	cText3    = rgb(0x7C7C86)
	cPrimary  = rgb(0x8B7CFF)
	cGood     = rgb(0x45D483)
	cWarn     = rgb(0xF5A524)
	cBad      = rgb(0xFF6369)
	cNeutral  = rgb(0x7C7C86)
	cBrand1   = rgb(0x2A2470)
	cBrand2   = rgb(0x4B2A8C)
	cMint     = rgb(0x7CF0C8)
)

const (
	cardW   = 1080
	cardPad = 56
)

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xFF}
}

// mix returns a over b with alpha t (0..1), opaque.
func mix(a, b color.RGBA, t float64) color.RGBA {
	f := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*t + float64(y)*(1-t))) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 0xFF}
}

// ---------- fonts ----------

var (
	fontOnce                     sync.Once
	fontReg, fontBold, fontTitle *opentype.Font
	faceMu                       sync.Mutex
	faces                        = map[string]font.Face{}
)

func loadCardFonts() {
	fontOnce.Do(func() {
		parse := func(name string) *opentype.Font {
			b, err := cardFonts.ReadFile("fonts/" + name)
			if err != nil {
				panic(err)
			}
			f, err := opentype.Parse(b)
			if err != nil {
				panic(err)
			}
			return f
		}
		fontReg, fontBold, fontTitle = parse("manrope-500.ttf"), parse("manrope-700.ttf"), parse("sora-600.ttf")
	})
}

type weight int

const (
	wReg weight = iota
	wBold
	wTitle
)

func face(w weight, size float64) font.Face {
	loadCardFonts()
	key := fmt.Sprintf("%d/%.1f", w, size)
	faceMu.Lock()
	defer faceMu.Unlock()
	if f, ok := faces[key]; ok {
		return f
	}
	src := map[weight]*opentype.Font{wReg: fontReg, wBold: fontBold, wTitle: fontTitle}[w]
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	faces[key] = f
	return f
}

// ---------- canvas ----------

type canvas struct{ img *image.RGBA }

func newCanvas(h int) *canvas {
	c := &canvas{image.NewRGBA(image.Rect(0, 0, cardW, h))}
	draw.Draw(c.img, c.img.Bounds(), image.NewUniform(cBg), image.Point{}, draw.Src)
	return c
}

// header paints the brand glow behind the title (night → indigo → violet, fading out).
func (c *canvas) header(h int) {
	for y := 0; y < h; y++ {
		fade := 1 - float64(y)/float64(h)
		fade = fade * fade * 0.55
		for x := 0; x < cardW; x++ {
			brand := mix(cBrand2, cBrand1, float64(x)/cardW)
			c.img.SetRGBA(x, y, mix(brand, cBg, fade))
		}
	}
}

func (c *canvas) png(h int) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, c.img.SubImage(image.Rect(0, 0, cardW, h))); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// path records an absolute-coordinate shape and rasterizes only its bounding box.
type path struct {
	ops                    []pathOp
	minX, minY, maxX, maxY float32
}

type pathOp struct {
	k byte // M L C Z
	p [3][2]float32
}

func (p *path) grow(x, y float32) {
	if len(p.ops) == 0 && p.minX == 0 && p.maxX == 0 {
		p.minX, p.minY, p.maxX, p.maxY = x, y, x, y
	}
	p.minX, p.minY = min(p.minX, x), min(p.minY, y)
	p.maxX, p.maxY = max(p.maxX, x), max(p.maxY, y)
}
func (p *path) moveTo(x, y float32) {
	p.grow(x, y)
	p.ops = append(p.ops, pathOp{k: 'M', p: [3][2]float32{{x, y}}})
}
func (p *path) lineTo(x, y float32) {
	p.grow(x, y)
	p.ops = append(p.ops, pathOp{k: 'L', p: [3][2]float32{{x, y}}})
}
func (p *path) cubeTo(x1, y1, x2, y2, x, y float32) {
	p.grow(x1, y1)
	p.grow(x2, y2)
	p.grow(x, y)
	p.ops = append(p.ops, pathOp{k: 'C', p: [3][2]float32{{x1, y1}, {x2, y2}, {x, y}}})
}
func (p *path) close() { p.ops = append(p.ops, pathOp{k: 'Z'}) }

func (c *canvas) fill(p *path, col color.RGBA) {
	if len(p.ops) == 0 {
		return
	}
	x0, y0 := int(math.Floor(float64(p.minX))), int(math.Floor(float64(p.minY)))
	x1, y1 := int(math.Ceil(float64(p.maxX))), int(math.Ceil(float64(p.maxY)))
	w, h := x1-x0, y1-y0
	if w <= 0 || h <= 0 {
		return
	}
	z := vector.NewRasterizer(w, h)
	fx, fy := float32(x0), float32(y0)
	for _, op := range p.ops {
		switch op.k {
		case 'M':
			z.MoveTo(op.p[0][0]-fx, op.p[0][1]-fy)
		case 'L':
			z.LineTo(op.p[0][0]-fx, op.p[0][1]-fy)
		case 'C':
			z.CubeTo(op.p[0][0]-fx, op.p[0][1]-fy, op.p[1][0]-fx, op.p[1][1]-fy, op.p[2][0]-fx, op.p[2][1]-fy)
		case 'Z':
			z.ClosePath()
		}
	}
	z.Draw(c.img, image.Rect(x0, y0, x1, y1), image.NewUniform(col), image.Point{})
}

const kappa = 0.5523

func roundRect(x, y, w, h, r float32) *path {
	r = min(r, w/2, h/2)
	k := r * kappa
	p := &path{}
	p.moveTo(x+r, y)
	p.lineTo(x+w-r, y)
	p.cubeTo(x+w-r+k, y, x+w, y+r-k, x+w, y+r)
	p.lineTo(x+w, y+h-r)
	p.cubeTo(x+w, y+h-r+k, x+w-r+k, y+h, x+w-r, y+h)
	p.lineTo(x+r, y+h)
	p.cubeTo(x+r-k, y+h, x, y+h-r+k, x, y+h-r)
	p.lineTo(x, y+r)
	p.cubeTo(x, y+r-k, x+r-k, y, x+r, y)
	p.close()
	return p
}

func circle(cx, cy, r float32) *path { return roundRect(cx-r, cy-r, 2*r, 2*r, r) }

// card draws a surface with a 1 px border, like .card in the panel.
func (c *canvas) card(x, y, w, h, r float32) {
	c.fill(roundRect(x, y, w, h, r), cBorder)
	c.fill(roundRect(x+1.5, y+1.5, w-3, h-3, r-1.5), cSurface)
}

// stroke draws a polyline of width w with round joins.
func (c *canvas) stroke(pts [][2]float32, w float32, col color.RGBA) {
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := float32(math.Hypot(float64(dx), float64(dy)))
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*w/2, dx/l*w/2
		p := &path{}
		p.moveTo(a[0]+nx, a[1]+ny)
		p.lineTo(b[0]+nx, b[1]+ny)
		p.lineTo(b[0]-nx, b[1]-ny)
		p.lineTo(a[0]-nx, a[1]-ny)
		p.close()
		c.fill(p, col)
	}
	for _, q := range pts {
		c.fill(circle(q[0], q[1], w/2), col)
	}
}

// ---------- text ----------

func textW(f font.Face, s string) int { return font.MeasureString(f, s).Round() }

// text draws s with its baseline at y; returns the advance.
func (c *canvas) text(f font.Face, x, y int, col color.RGBA, s string) int {
	d := font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
	return (d.Dot.X - fixed.I(x)).Round()
}

// textR draws s right-aligned to x.
func (c *canvas) textR(f font.Face, x, y int, col color.RGBA, s string) {
	c.text(f, x-textW(f, s), y, col, s)
}

// fit shortens s with "…" so it fits in maxW pixels.
func fit(f font.Face, s string, maxW int) string {
	if textW(f, s) <= maxW {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && textW(f, string(r)+"…") > maxW {
		r = r[:len(r)-1]
	}
	return strings.TrimSpace(string(r)) + "…"
}

// ---------- components ----------

func toneColor(t string) color.RGBA {
	switch t {
	case "good":
		return cGood
	case "warn":
		return cWarn
	case "bad":
		return cBad
	case "primary":
		return cPrimary
	}
	return cNeutral
}

// chip draws a status pill (dot + label), like .chip; returns its width.
func (c *canvas) chip(x, y int, tone, label string, size float64) int {
	f := face(wBold, size)
	col := toneColor(tone)
	h := int(size * 1.9)
	dot := int(size * 0.42)
	w := int(size*0.9) + dot + int(size*0.45) + textW(f, label) + int(size*0.9)
	c.fill(roundRect(float32(x), float32(y), float32(w), float32(h), float32(h)/2), mix(col, cSurface, 0.16))
	cx := float32(x) + float32(size*0.9) + float32(dot)/2
	c.fill(circle(cx, float32(y)+float32(h)/2, float32(dot)/2), col)
	c.text(f, x+int(size*0.9)+dot+int(size*0.45), y+h/2+int(size*0.36), mix(col, cText, 0.88), label)
	return w
}

// meter draws a rounded bar (track + value).
func (c *canvas) meter(x, y, w, h int, v float64, hot bool) {
	c.fill(roundRect(float32(x), float32(y), float32(w), float32(h), float32(h)/2), cTrack)
	if v <= 0 {
		return
	}
	fw := float32(w) * float32(math.Min(v, 100)) / 100
	fw = max(fw, float32(h))
	col := cPrimary
	if hot {
		col = cBad
	}
	c.fill(roundRect(float32(x), float32(y), fw, float32(h), float32(h)/2), col)
}

// logo draws the small app icon (squircle + star + mint node), like favicon.svg.
func (c *canvas) logo(x, y, s float32) {
	c.fill(roundRect(x, y, s, s, s*0.22), cBrand1)
	u := s / 64
	p := &path{}
	p.moveTo(x+32*u, y+9*u)
	p.cubeTo(x+34*u, y+26*u, x+38*u, y+30*u, x+54*u, y+32*u)
	p.cubeTo(x+38*u, y+34*u, x+34*u, y+38*u, x+32*u, y+55*u)
	p.cubeTo(x+30*u, y+38*u, x+26*u, y+34*u, x+10*u, y+32*u)
	p.cubeTo(x+26*u, y+30*u, x+30*u, y+26*u, x+32*u, y+9*u)
	p.close()
	c.fill(p, cText)
	c.fill(circle(x+51*u, y+14*u, 5*u), cMint)
}

// brandRow: logo + "Astrea" on the left, a caption on the right (e.g. the time).
func (c *canvas) brandRow(y int, right string) {
	c.logo(cardPad, float32(y), 44)
	c.text(face(wTitle, 28), cardPad+60, y+32, cText, "Astrea")
	c.textR(face(wReg, 24), cardW-cardPad, y+31, cText2, right)
}

// area draws a 0–100 series as an area chart in (x,y,w,h). The x axis spans
// `slots` samples anchored to the right ("ahora"); negative values are gaps.
func (c *canvas) area(x, y, w, h int, vals []float64, slots int) {
	// recessive grid: baseline + 50 %
	c.fill(roundRect(float32(x), float32(y+h), float32(w), 1.5, 0), cBorder)
	for xx := x; xx < x+w; xx += 14 {
		c.fill(roundRect(float32(xx), float32(y+h/2), 6, 1.5, 0), cBorder)
	}
	if slots < 2 {
		slots = 2
	}
	off := slots - len(vals)
	px := func(i int) float32 { return float32(x) + float32(off+i)*float32(w)/float32(slots-1) }
	py := func(v float64) float32 { return float32(y+h) - float32(math.Min(v, 100)/100)*float32(h-4) }
	area := mix(cPrimary, cSurface, 0.22)
	run := [][2]float32{}
	flush := func() {
		switch {
		case len(run) == 1:
			c.fill(circle(run[0][0], run[0][1], 4), cPrimary)
		case len(run) > 1:
			p := &path{}
			p.moveTo(run[0][0], float32(y+h))
			for _, q := range run {
				p.lineTo(q[0], q[1])
			}
			p.lineTo(run[len(run)-1][0], float32(y+h))
			p.close()
			c.fill(p, area)
			c.stroke(run, 3.5, cPrimary)
		}
		run = run[:0]
	}
	for i, v := range vals {
		if v < 0 {
			flush()
			continue
		}
		run = append(run, [2]float32{px(i), py(v)})
	}
	flush()
}

// ---------- data ----------

type cardStats struct {
	Host struct {
		Hostname        string  `json:"hostname"`
		Platform        string  `json:"platform"`
		PlatformVersion string  `json:"platform_version"`
		UptimeSec       float64 `json:"uptime_sec"`
	} `json:"host"`
	CPU struct {
		Model   string   `json:"model"`
		Usage   *float64 `json:"usage_percent"`
		Cores   int      `json:"cores"`
		Threads int      `json:"threads"`
	} `json:"cpu"`
	Memory struct {
		Total, Used uint64
		UsedPct     *float64 `json:"used_percent"`
	} `json:"memory"`
	GPUs  []cardGPU `json:"gpus"`
	Disks []struct {
		Mount       string `json:"mount"`
		Total, Used uint64
		UsedPct     float64 `json:"used_percent"`
	} `json:"disks"`
}

type cardGPU struct {
	Name      string   `json:"name"`
	Vendor    string   `json:"vendor"`
	Usage     *float64 `json:"usage_percent"`
	VRAMUsed  *uint64  `json:"vram_used"`
	VRAMTotal uint64   `json:"vram_total"`
	TempC     *float64 `json:"temperature_c"`
	Shared    bool     `json:"shared_memory"`
}

// pickGPU chooses the GPU worth showing: one that reports load, else the one
// with most memory (a discrete NVIDIA over the integrated Intel). -1 if none.
func pickGPU(g []cardGPU) int {
	best := -1
	for i, x := range g {
		switch {
		case best < 0:
			best = i
		case (x.Usage != nil) != (g[best].Usage != nil):
			if x.Usage != nil {
				best = i
			}
		case x.VRAMTotal > g[best].VRAMTotal:
			best = i
		}
	}
	return best
}

type cardMachine struct {
	Name, OS, Tone, Label, Detail string
	Online, Locked                bool
	Stats                         *cardStats
	Running, Services             int
}

func cardFrom(s machineStatus) cardMachine {
	m := cardMachine{Name: s.Name, Online: s.Online}
	if len(s.Stats) > 0 {
		var st cardStats
		if json.Unmarshal(s.Stats, &st) == nil {
			m.Stats = &st
			m.OS = strings.TrimSpace(st.Host.Platform + " " + st.Host.PlatformVersion)
			if m.Name == "" {
				m.Name = st.Host.Hostname
			}
		}
	}
	var admin struct {
		Locked bool `json:"locked"`
	}
	_ = json.Unmarshal(s.Admin, &admin)
	m.Locked = admin.Locked
	var idle struct {
		Session    string  `json:"session"`
		UserActive bool    `json:"user_active"`
		IdleSec    float64 `json:"idle_sec"`
		Process    string  `json:"process"`
	}
	hasIdle := json.Unmarshal(s.Idle, &idle) == nil && len(s.Idle) > 0
	var svc struct {
		Services []struct {
			State string `json:"state"`
		} `json:"services"`
	}
	_ = json.Unmarshal(s.Services, &svc)
	m.Services = len(svc.Services)
	for _, x := range svc.Services {
		if x.State == "running" {
			m.Running++
		}
	}
	// Same verdict as presence() in web/index.html.
	switch {
	case !s.Online:
		m.Tone, m.Label = "bad", "Sin conexión"
		if s.LastSeen > 0 {
			m.Detail = "Vista " + cardAgo(time.Since(time.UnixMilli(s.LastSeen)))
		} else {
			m.Detail = "No responde"
		}
	case m.Locked:
		m.Tone, m.Label, m.Detail = "bad", "Bloqueada", "Freno de emergencia activado"
	case !hasIdle:
		m.Tone, m.Label = "good", "En línea"
	case idle.Session == "none":
		m.Tone, m.Label, m.Detail = "good", "Libre", "Nadie ha iniciado sesión"
	case idle.UserActive:
		m.Tone, m.Label, m.Detail = "warn", "En uso", "Alguien la está usando"
		if idle.Process != "" {
			m.Detail += " · " + idle.Process
		}
	default:
		m.Tone, m.Label = "good", "Libre"
		m.Detail = "Sin actividad desde " + cardAgo(time.Duration(idle.IdleSec)*time.Second)
	}
	return m
}

func cardAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("hace %d s", max(1, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("hace %d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("hace %d h", int(d.Hours()))
	}
	return fmt.Sprintf("hace %d d", int(d.Hours()/24))
}

func cardUptime(sec float64) string {
	d, h, m := int(sec)/86400, int(sec)%86400/3600, int(sec)%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%d d %d h", d, h)
	case h > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%d min", m)
}

func cardBytes(n uint64) string {
	u := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if v >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", v, u[i])
	}
	return fmt.Sprintf("%.1f %s", v, u[i])
}

func pctS(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", *v)
}

func val(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func fptr(f float64) *float64 { return &f }

// ---------- machine card ----------

// renderMachineCard draws one machine: status, CPU/RAM/GPU with their recent
// history, disks and services.
func renderMachineCard(s machineStatus, h cardHistory) ([]byte, error) {
	m := cardFrom(s)
	c := newCanvas(2400)
	c.header(420)
	c.brandRow(cardPad, time.Now().Format("15:04"))

	y := cardPad + 44 + 70
	c.text(face(wTitle, 60), cardPad, y+50, cText, fit(face(wTitle, 60), m.Name, cardW-2*cardPad))
	y += 82
	cw := c.chip(cardPad, y, m.Tone, m.Label, 24)
	if m.Detail != "" {
		c.text(face(wReg, 26), cardPad+cw+16, y+32, cText2, fit(face(wReg, 26), m.Detail, cardW-2*cardPad-cw-16))
	}
	y += 46 + 18
	sub := m.OS
	if m.Stats != nil && m.Online && m.Stats.Host.UptimeSec > 0 {
		sub += " · encendido hace " + cardUptime(m.Stats.Host.UptimeSec)
	}
	if sub != "" {
		c.text(face(wReg, 24), cardPad, y+24, cText3, fit(face(wReg, 24), sub, cardW-2*cardPad))
		y += 40
	}
	y += 28

	slots := historyLen
	window := time.Duration(slots) * h.Step
	st := m.Stats
	if st == nil {
		st = &cardStats{}
	}

	// GPU tile data
	gi := pickGPU(st.GPUs)
	var gpuVal *float64
	gpuTitle, gpuModel, gpuDetail := "Gráfica", "", ""
	if gi >= 0 {
		g := st.GPUs[gi]
		gpuModel = g.Name
		if g.Vendor != "" {
			gpuTitle += " · " + g.Vendor
		}
		var parts []string
		if g.VRAMUsed != nil && g.VRAMTotal > 0 {
			vp := float64(*g.VRAMUsed) / float64(g.VRAMTotal) * 100
			if g.Usage == nil {
				gpuVal = fptr(vp)
			}
			parts = append(parts, fmt.Sprintf("VRAM %s de %s", cardBytes(*g.VRAMUsed), cardBytes(g.VRAMTotal)))
		}
		if g.Usage != nil {
			gpuVal = g.Usage
		}
		if g.TempC != nil {
			parts = append(parts, fmt.Sprintf("%.0f °C", *g.TempC))
		}
		gpuDetail = strings.Join(parts, " · ")
	}

	type tile struct {
		title, model, detail string
		v                    *float64
		hist                 []float64
	}
	tiles := []tile{
		{"Procesador", st.CPU.Model, fmt.Sprintf("%d núcleos · %d hilos", st.CPU.Cores, st.CPU.Threads), st.CPU.Usage, h.CPU},
		{"Memoria", cardBytes(st.Memory.Total) + " instalados", cardBytes(st.Memory.Used) + " en uso", st.Memory.UsedPct, h.RAM},
	}
	if gi >= 0 || hasData(h.GPU) {
		tiles = append(tiles, tile{gpuTitle, gpuModel, gpuDetail, gpuVal, h.GPU})
	}
	if m.Stats == nil {
		for i := range tiles {
			tiles[i].model, tiles[i].detail, tiles[i].v = "", "", nil
		}
	}

	for _, t := range tiles {
		th := 330
		x, w := cardPad, cardW-2*cardPad
		c.card(float32(x), float32(y), float32(w), float32(th), 30)
		ix := x + 32
		c.text(face(wBold, 26), ix, y+50, cText2, t.title)
		if t.model != "" {
			c.textR(face(wReg, 22), x+w-32, y+50, cText3, fit(face(wReg, 22), t.model, w-64-textW(face(wBold, 26), t.title)-24))
		}
		big := "—"
		if t.v != nil {
			big = fmt.Sprintf("%.0f%%", *t.v)
		}
		bw := c.text(face(wTitle, 64), ix, y+124, cText, big)
		if t.detail != "" {
			c.text(face(wReg, 24), ix+bw+18, y+122, cText2, fit(face(wReg, 24), t.detail, w-64-bw-18))
		}
		// chart
		chX, chY, chW, chH := ix, y+150, w-64, 120
		if peak, ok := maxOf(t.hist); ok {
			c.textR(face(wReg, 20), chX+chW, chY+2, cText3, fmt.Sprintf("máx %.0f%%", peak))
		}
		if hasData(t.hist) {
			c.area(chX, chY+12, chW, chH-12, t.hist, slots)
		} else {
			c.fill(roundRect(float32(chX), float32(chY+chH), float32(chW), 1.5, 0), cBorder)
			c.text(face(wReg, 22), chX, chY+chH/2+8, cText3, "Recogiendo datos…")
		}
		c.text(face(wReg, 20), chX, chY+chH+30, cText3, "hace "+humanWindow(window))
		c.textR(face(wReg, 20), chX+chW, chY+chH+30, cText3, "ahora")
		y += th + 20
	}

	// disks + services
	if len(st.Disks) > 0 || m.Services > 0 {
		x, w := cardPad, cardW-2*cardPad
		dh := 70 + len(st.Disks)*62
		if m.Services > 0 {
			dh += 72
		}
		c.card(float32(x), float32(y), float32(w), float32(dh), 30)
		yy := y + 50
		c.text(face(wBold, 26), x+32, yy, cText2, "Almacenamiento")
		yy += 22
		for _, d := range st.Disks {
			c.text(face(wReg, 22), x+32, yy+24, cText, d.Mount)
			c.textR(face(wReg, 22), x+w-32, yy+24, cText2, cardBytes(d.Total-d.Used)+" libres de "+cardBytes(d.Total))
			c.meter(x+32, yy+38, w-64, 10, d.UsedPct, d.UsedPct >= 90)
			yy += 62
		}
		if m.Services > 0 {
			c.fill(roundRect(float32(x+32), float32(yy+6), float32(w-64), 1.5, 0), cBorder)
			txt := fmt.Sprintf("%d de %d servicios funcionando", m.Running, m.Services)
			tone := "good"
			if m.Running < m.Services {
				tone = "warn"
			}
			c.fill(circle(float32(x+40), float32(yy+38), 6), toneColor(tone))
			c.text(face(wReg, 22), x+56, yy+46, cText2, txt)
		}
		y += dh + 20
	}

	y += 12
	c.text(face(wReg, 20), cardPad, y+20, cText3, "Astrea · actualizado "+time.Now().Format("15:04:05"))
	return c.png(y + 20 + cardPad - 8)
}

func hasData(v []float64) bool {
	for _, x := range v {
		if x >= 0 {
			return true
		}
	}
	return false
}

func maxOf(v []float64) (float64, bool) {
	m, ok := 0.0, false
	for _, x := range v {
		if x >= 0 && (!ok || x > m) {
			m, ok = x, true
		}
	}
	return m, ok
}

func humanWindow(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.0f h", d.Hours())
	}
	return fmt.Sprintf("%.0f min", d.Minutes())
}

// ---------- fleet card ----------

// renderFleetCard draws the whole fleet: KPIs and one row per machine.
func renderFleetCard(list []machineStatus) ([]byte, error) {
	ms := make([]cardMachine, len(list))
	online, free, busy, running := 0, 0, 0, 0
	for i, s := range list {
		ms[i] = cardFrom(s)
		if ms[i].Online {
			online++
		}
		switch ms[i].Label {
		case "Libre":
			free++
		case "En uso":
			busy++
		}
		running += ms[i].Running
	}

	c := newCanvas(cardPad*2 + 400 + len(ms)*236 + 200)
	c.header(380)
	c.brandRow(cardPad, time.Now().Format("15:04"))

	y := cardPad + 44 + 70
	c.text(face(wTitle, 60), cardPad, y+50, cText, "Tu flota")
	y += 92
	c.text(face(wReg, 26), cardPad, y+10, cText2, fmt.Sprintf("%d de %d equipos en línea", online, len(ms)))
	y += 48

	// KPI tiles
	kpis := []struct{ label, value, of string }{
		{"En línea", fmt.Sprint(online), fmt.Sprintf("/ %d", len(ms))},
		{"Libres", fmt.Sprint(free), ""},
		{"En uso", fmt.Sprint(busy), ""},
		{"Servicios", fmt.Sprint(running), ""},
	}
	gap := 16
	kw := (cardW - 2*cardPad - 3*gap) / 4
	for i, k := range kpis {
		x := cardPad + i*(kw+gap)
		c.card(float32(x), float32(y), float32(kw), 150, 26)
		c.text(face(wBold, 22), x+24, y+44, cText2, k.label)
		vw := c.text(face(wTitle, 56), x+24, y+118, cText, k.value)
		if k.of != "" {
			c.text(face(wBold, 28), x+24+vw+8, y+118, cText3, k.of)
		}
	}
	y += 150 + 36

	if len(ms) == 0 {
		c.card(cardPad, float32(y), cardW-2*cardPad, 160, 30)
		c.text(face(wBold, 28), cardPad+32, y+70, cText, "Todavía no hay equipos")
		c.text(face(wReg, 22), cardPad+32, y+110, cText2, "Añade uno desde el panel: Ayuda › Añadir un equipo.")
		y += 180
	}

	for _, m := range ms {
		x, w, rh := cardPad, cardW-2*cardPad, 216
		c.card(float32(x), float32(y), float32(w), float32(rh), 30)
		// left: identity
		lx, lw := x+32, 420
		c.text(face(wTitle, 34), lx, y+60, cText, fit(face(wTitle, 34), m.Name, lw))
		c.chip(lx, y+84, m.Tone, m.Label, 20)
		sub := m.OS
		if !m.Online {
			sub = m.Detail
		}
		if sub == "" {
			sub = "—"
		}
		c.text(face(wReg, 21), lx, y+170, cText3, fit(face(wReg, 21), sub, lw))
		// right: bars
		bx, bw := x+w-32-440, 440
		if m.Online && m.Stats != nil {
			st := m.Stats
			rows := []struct {
				k string
				v *float64
			}{{"CPU", st.CPU.Usage}, {"RAM", st.Memory.UsedPct}}
			if gi := pickGPU(st.GPUs); gi >= 0 {
				g := st.GPUs[gi]
				if g.Usage != nil {
					rows = append(rows, struct {
						k string
						v *float64
					}{"GPU", g.Usage})
				} else if g.VRAMUsed != nil && g.VRAMTotal > 0 {
					rows = append(rows, struct {
						k string
						v *float64
					}{"VRAM", fptr(float64(*g.VRAMUsed) / float64(g.VRAMTotal) * 100)})
				}
			}
			ry := y + 46
			for _, r := range rows {
				c.text(face(wBold, 21), bx, ry+8, cText3, r.k)
				c.meter(bx+76, ry-2, bw-76-76, 10, val(r.v), val(r.v) >= 92)
				c.textR(face(wBold, 22), bx+bw, ry+9, cText2, pctS(r.v))
				ry += 52
			}
		} else {
			c.text(face(wReg, 22), bx, y+118, cText3, "Sin datos mientras no responda")
		}
		y += rh + 20
	}

	y += 12
	c.text(face(wReg, 20), cardPad, y+20, cText3, "Astrea · actualizado "+time.Now().Format("15:04:05"))
	return c.png(y + 20 + cardPad - 8)
}
