package capability

import (
	"context"
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"testing"
)

func call(t *testing.T, c Capability, payload string) map[string]any {
	t.Helper()
	res, err := c.Invoke(context.Background(), json.RawMessage(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestFSWriteReadText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "a.txt")
	pj, _ := json.Marshal(p)
	call(t, FSWrite{}, `{"path":`+string(pj)+`,"content":"hola ñandú\n"}`)
	m := call(t, FSRead{}, `{"path":`+string(pj)+`}`)
	if m["content"] != "hola ñandú\n" || m["truncated"] != false {
		t.Fatalf("text round-trip failed: %v", m)
	}
	m = call(t, FSRead{}, `{"path":`+string(pj)+`,"offset":5,"limit":3}`)
	if m["content"] != "ña" || m["truncated"] != true { // "ñ" is 2 bytes
		t.Fatalf("paging failed: %v", m)
	}
}

func TestFSReadBinaryIsBase64(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.bin")
	if err := os.WriteFile(p, []byte{0xff, 0xfe, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	pj, _ := json.Marshal(p)
	m := call(t, FSRead{}, `{"path":`+string(pj)+`}`)
	if m["base64"] != "//4AAQ==" || m["content"] != nil {
		t.Fatalf("binary should come back as base64: %v", m)
	}
}

func TestDownscale(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	dst := downscale(src, 2, 1)
	if dst.Bounds().Dx() != 2 || dst.Bounds().Dy() != 1 || dst.Pix[0] != 200 || dst.Pix[3] != 255 {
		t.Fatalf("unexpected downscale: %v %v", dst.Bounds(), dst.Pix)
	}
}
