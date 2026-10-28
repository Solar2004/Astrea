package capability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// Audio lets the agent listen to the controlled machine's microphone and read
// or set its input level. Capture records a few seconds from the default
// recording device and returns it as a WAV the model hears natively (like
// screen.capture returns a picture); audio.mic reads or moves the mic slider.
//
// The backend needs the OS audio stack, so it is Windows-only for now (WASAPI
// capture + Core Audio for the level); elsewhere both answer "unavailable".

// Audio is an audio clip attached to a capability result. The hub turns every
// entry of a result's top-level "audio" array into native MCP audio content,
// so the model hears the clip instead of a base64 string.
type Audio struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
}

// audioBackend captures from the default recording device and reads/sets its
// level. Capture returns interleaved 16-bit PCM samples.
type audioBackend interface {
	Capture(ctx context.Context, seconds int) (pcm []int16, sampleRate, channels int, err error)
	MicLevel() (level int, mute bool, err error)
	SetMicLevel(level int, mute *bool) error
}

// audioDev is the active backend, installed by audio_windows.go at init. It is
// nil on builds without an audio backend, and the capabilities then answer
// "unavailable".
var audioDev audioBackend

func requireAudio() error {
	if audioDev == nil {
		return &protocol.Error{Code: protocol.CodeUnavailable, Message: "audio is only available on Windows"}
	}
	return nil
}

// AudioCapture records a few seconds from the controlled machine's microphone.
type AudioCapture struct{}

func (AudioCapture) Name() string { return "audio.capture" }

func (AudioCapture) Describe() Descriptor {
	return Descriptor{
		Name: "audio.capture",
		Description: "Record from the controlled machine's default microphone and return it as a WAV clip " +
			"you can listen to. Use gain to amplify a quiet mic, or normalize to auto-level it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"seconds": {"type": "integer", "description": "How many seconds to record. Default 5, max 30."},
				"gain": {"type": "number", "description": "Amplify the captured audio by this factor before returning it (1 = unchanged, 2 = twice as loud). Default 1. Ignored when normalize is true."},
				"normalize": {"type": "boolean", "description": "Auto-amplify so the loudest part peaks near full scale. Overrides gain. Default false."}
			}
		}`),
	}
}

func (AudioCapture) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireAudio(); err != nil {
		return nil, err
	}
	req := struct {
		Seconds   int     `json:"seconds"`
		Gain      float64 `json:"gain"`
		Normalize bool    `json:"normalize"`
	}{Seconds: 5, Gain: 1}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}
	if req.Seconds <= 0 {
		req.Seconds = 5
	}
	if req.Seconds > 30 {
		req.Seconds = 30
	}

	pcm, rate, channels, err := audioDev.Capture(ctx, req.Seconds)
	if err != nil {
		return nil, internal(err)
	}

	gain := req.Gain
	if req.Normalize {
		gain = normalizeGain(pcm)
	} else if gain <= 0 {
		gain = 1
	}
	peak, clipped := applyGain(pcm, gain)

	wav := encodeWAV(pcm, rate, channels)
	return map[string]any{
		"seconds":     req.Seconds,
		"sample_rate": rate,
		"channels":    channels,
		"gain":        gain,
		"peak":        math.Round(peak*1000) / 1000, // 0..1 after gain
		"clipped":     clipped,
		"bytes":       len(wav),
		"audio":       []Audio{{MimeType: "audio/wav", Data: base64.StdEncoding.EncodeToString(wav)}},
	}, nil
}

// MicVolume reads the mic input level, or moves it (absolute, relative, mute).
type MicVolume struct{}

func (MicVolume) Name() string { return "audio.mic" }

func (MicVolume) Describe() Descriptor {
	return Descriptor{
		Name: "audio.mic",
		Description: "Read the microphone input level (0-100) and mute state of the controlled machine, " +
			"or change it. With no fields it only reads. Set level for an absolute level, delta to nudge it " +
			"(e.g. +10), or mute to mute/unmute.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"level": {"type": "integer", "description": "Set the input level to this value, 0-100."},
				"delta": {"type": "integer", "description": "Change the level by this many points (e.g. 10 louder, -10 quieter)."},
				"mute": {"type": "boolean", "description": "Mute (true) or unmute (false) the microphone."}
			}
		}`),
	}
}

func (MicVolume) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireAudio(); err != nil {
		return nil, err
	}
	req := struct {
		Level *int  `json:"level"`
		Delta *int  `json:"delta"`
		Mute  *bool `json:"mute"`
	}{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}

	if req.Level != nil || req.Delta != nil || req.Mute != nil {
		level := -1
		if req.Level != nil {
			level = clampLevel(*req.Level)
		} else if req.Delta != nil {
			cur, _, err := audioDev.MicLevel()
			if err != nil {
				return nil, internal(err)
			}
			level = clampLevel(cur + *req.Delta)
		}
		if err := audioDev.SetMicLevel(level, req.Mute); err != nil {
			return nil, internal(err)
		}
	}

	level, mute, err := audioDev.MicLevel()
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"level": level, "mute": mute}, nil
}

func clampLevel(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// applyGain multiplies every sample by gain in place, clamping to int16 range,
// and reports the resulting peak (0..1) and whether any sample clipped.
func applyGain(pcm []int16, gain float64) (peak float64, clipped bool) {
	var maxAbs int32
	for i, s := range pcm {
		v := int32(math.Round(float64(s) * gain))
		if v > 32767 {
			v = 32767
			clipped = true
		} else if v < -32768 {
			v = -32768
			clipped = true
		}
		pcm[i] = int16(v)
		if a := v; a < 0 {
			a = -a
			if a > maxAbs {
				maxAbs = a
			}
		} else if a > maxAbs {
			maxAbs = a
		}
	}
	return float64(maxAbs) / 32768, clipped
}

// normalizeGain returns the gain that brings the clip's peak to ~0.97 of full
// scale. It returns 1 for silence (no peak to scale from).
func normalizeGain(pcm []int16) float64 {
	var maxAbs int32
	for _, s := range pcm {
		a := int32(s)
		if a < 0 {
			a = -a
		}
		if a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		return 1
	}
	return 0.97 * 32768 / float64(maxAbs)
}

// encodeWAV wraps interleaved 16-bit PCM in a canonical 44-byte RIFF/WAVE
// header.
func encodeWAV(pcm []int16, sampleRate, channels int) []byte {
	if channels < 1 {
		channels = 1
	}
	if sampleRate < 1 {
		sampleRate = 48000
	}
	dataLen := len(pcm) * 2
	byteRate := sampleRate * channels * 2
	blockAlign := channels * 2

	var buf bytes.Buffer
	buf.Grow(44 + dataLen)
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	w(uint32(36 + dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	w(uint32(16))           // PCM fmt chunk size
	w(uint16(1))            // audio format = PCM
	w(uint16(channels))     //
	w(uint32(sampleRate))   //
	w(uint32(byteRate))     //
	w(uint16(blockAlign))   //
	w(uint16(16))           // bits per sample
	buf.WriteString("data") //
	w(uint32(dataLen))      //
	for _, s := range pcm { //
		w(s) //
	}
	return buf.Bytes()
}
