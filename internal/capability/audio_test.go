package capability

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeWAVHeader(t *testing.T) {
	pcm := []int16{0, 100, -100, 32767, -32768}
	wav := encodeWAV(pcm, 48000, 2)
	if len(wav) != 44+len(pcm)*2 {
		t.Fatalf("len = %d, want %d", len(wav), 44+len(pcm)*2)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[36:40]) != "data" {
		t.Fatalf("bad RIFF/WAVE/data markers: %q", wav[:44])
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 2 {
		t.Errorf("channels = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 48000 {
		t.Errorf("sample rate = %d, want 48000", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); int(got) != len(pcm)*2 {
		t.Errorf("data size = %d, want %d", got, len(pcm)*2)
	}
	// Samples round-trip little-endian.
	var sample int16
	binary.Read(bytes.NewReader(wav[44:46]), binary.LittleEndian, &sample)
	if sample != 0 {
		t.Errorf("first sample = %d, want 0", sample)
	}
}

func TestEncodeWAVDefaults(t *testing.T) {
	wav := encodeWAV(nil, 0, 0)
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels fallback = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 48000 {
		t.Errorf("rate fallback = %d, want 48000", got)
	}
}

func TestApplyGain(t *testing.T) {
	pcm := []int16{100, -100, 200}
	peak, clipped := applyGain(pcm, 2)
	if clipped {
		t.Error("should not clip")
	}
	if pcm[0] != 200 || pcm[1] != -200 || pcm[2] != 400 {
		t.Errorf("gain not applied: %v", pcm)
	}
	wantPeak := 400.0 / 32768
	if peak < wantPeak-1e-6 || peak > wantPeak+1e-6 {
		t.Errorf("peak = %v, want %v", peak, wantPeak)
	}
}

func TestApplyGainClips(t *testing.T) {
	pcm := []int16{30000, -30000}
	peak, clipped := applyGain(pcm, 4)
	if !clipped {
		t.Error("should clip")
	}
	if pcm[0] != 32767 || pcm[1] != -32768 {
		t.Errorf("not clamped: %v", pcm)
	}
	if peak <= 0.99 {
		t.Errorf("peak = %v, want ~1", peak)
	}
}

func TestNormalizeGain(t *testing.T) {
	if g := normalizeGain([]int16{0, 0, 0}); g != 1 {
		t.Errorf("silence gain = %v, want 1", g)
	}
	// Peak 16384 (half scale) should roughly double toward 0.97 full scale.
	g := normalizeGain([]int16{16384, -8000})
	want := 0.97 * 32768 / 16384
	if g < want-1e-6 || g > want+1e-6 {
		t.Errorf("gain = %v, want %v", g, want)
	}
}
