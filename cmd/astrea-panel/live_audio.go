// Escucha en vivo del micrófono de un equipo, análogo a la vista en vivo de
// pantalla (live.go): en vez de encadenar screen.capture como vídeo MJPEG,
// encadena audio.capture como un stream WAV continuo que el navegador reproduce
// con un <audio>.
//
//	GET  /api/mic?machine=&gain=&secs=   audio/wav en streaming (cabecera una vez, luego PCM)
//	POST /api/mic/level {machine, level?, delta?, mute?}   lee o ajusta el nivel del micro
//	GET  /api/mic/level?machine=         solo lee el nivel
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// micResult es la respuesta de audio.capture: el clip va en audio[0].data como
// WAV en base64, con su frecuencia y nº de canales aparte.
type micResult struct {
	SampleRate int `json:"sample_rate"`
	Channels   int `json:"channels"`
	Audio      []struct {
		Data string `json:"data"`
	} `json:"audio"`
}

// handleMic transmite el micrófono del equipo como un único stream WAV: captura
// en trozos cortos y los concatena. Entre trozos puede haber un hueco mínimo
// (el tiempo de ida y vuelta por el tailnet); para voz de aula es suficiente.
func (p *panel) handleMic(w http.ResponseWriter, r *http.Request) {
	machine := r.URL.Query().Get("machine")
	if p.client(machine) == nil {
		http.Error(w, errUnknownMachine.Error(), http.StatusNotFound)
		return
	}
	gain, _ := strconv.ParseFloat(r.URL.Query().Get("gain"), 64)
	if gain <= 0 {
		gain = 1
	}
	secs := queryInt(r, "secs", 1, 1, 5)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming no soportado", http.StatusInternalServerError)
		return
	}
	wroteHeader := false
	fails := 0
	for {
		if r.Context().Err() != nil {
			return
		}
		var res micResult
		code, err := p.call(r.Context(), machine, "audio.capture",
			map[string]any{"seconds": secs, "gain": gain}, &res, time.Duration(secs+10)*time.Second)
		if err != nil || len(res.Audio) == 0 {
			if !wroteHeader {
				if err == nil {
					code, err = http.StatusBadGateway, errors.New("el micrófono no devolvió audio")
				}
				fail(w, code, err)
				return
			}
			// Ya transmitiendo: un fallo suelto se reintenta; varios seguidos cierran.
			if fails++; fails > 3 {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		fails = 0
		wav, derr := base64.StdEncoding.DecodeString(res.Audio[0].Data)
		if derr != nil {
			return
		}
		pcm := pcmData(wav)
		if !wroteHeader {
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Cache-Control", "no-store")
			if _, err := w.Write(wavStreamHeader(res.SampleRate, res.Channels)); err != nil {
				return
			}
			wroteHeader = true
		}
		if _, err := w.Write(pcm); err != nil {
			return
		}
		flusher.Flush()
	}
}

// handleMicLevel lee (GET, o POST sin campos) o ajusta (POST) el nivel del micro.
func (p *panel) handleMicLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Machine string `json:"machine"`
		Level   *int   `json:"level,omitempty"`
		Delta   *int   `json:"delta,omitempty"`
		Mute    *bool  `json:"mute,omitempty"`
	}
	if r.Method == http.MethodGet {
		req.Machine = r.URL.Query().Get("machine")
	} else if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	} else {
		fail(w, http.StatusMethodNotAllowed, errUnknownMachine)
		return
	}

	args := map[string]any{}
	if req.Level != nil {
		args["level"] = *req.Level
	}
	if req.Delta != nil {
		args["delta"] = *req.Delta
	}
	if req.Mute != nil {
		args["mute"] = *req.Mute
	}
	var out struct {
		Level int  `json:"level"`
		Mute  bool `json:"mute"`
	}
	if code, err := p.call(r.Context(), req.Machine, "audio.mic", args, &out, 10*time.Second); err != nil {
		fail(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"level": out.Level, "mute": out.Mute})
}

// pcmData devuelve el bloque de muestras PCM de un WAV, saltándose la cabecera:
// localiza el subchunk "data" en vez de asumir 44 bytes fijos.
func pcmData(wav []byte) []byte {
	i := bytes.Index(wav, []byte("data"))
	if i < 0 || i+8 > len(wav) {
		return nil
	}
	return wav[i+8:]
}

// wavStreamHeader arma una cabecera WAV de 44 bytes con los tamaños a 0xFFFFFFFF
// (longitud "infinita"), para que el navegador empiece a reproducir un stream
// cuya duración no se conoce de antemano.
func wavStreamHeader(sampleRate, channels int) []byte {
	if channels < 1 {
		channels = 1
	}
	if sampleRate < 1 {
		sampleRate = 48000
	}
	byteRate := sampleRate * channels * 2
	blockAlign := channels * 2
	var buf bytes.Buffer
	wr := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	wr(uint32(0xFFFFFFFF))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	wr(uint32(16))
	wr(uint16(1)) // PCM
	wr(uint16(channels))
	wr(uint32(sampleRate))
	wr(uint32(byteRate))
	wr(uint16(blockAlign))
	wr(uint16(16)) // bits
	buf.WriteString("data")
	wr(uint32(0xFFFFFFFF))
	return buf.Bytes()
}
