package capability

import "testing"

func TestMatchWindows(t *testing.T) {
	ws := []Window{
		{Title: "YouTube — Zen Browser", Process: "zen.exe"},
		{Title: "Documentos", Process: "explorer.exe"},
		{Title: "Zen notes.txt - Notepad", Process: "notepad.exe"},
	}
	if m := matchWindows(ws, "", "ZEN"); len(m) != 1 || m[0].Process != "zen.exe" {
		t.Fatalf("by process: %+v", m)
	}
	if m := matchWindows(ws, "zen", ""); len(m) != 2 || m[0].Process != "zen.exe" {
		t.Fatalf("by title, frontmost first: %+v", m)
	}
	if m := matchWindows(ws, "zen", "notepad.exe"); len(m) != 1 || m[0].Process != "notepad.exe" {
		t.Fatalf("title + process: %+v", m)
	}
	if m := matchWindows(ws, "word", ""); len(m) != 0 {
		t.Fatalf("no match: %+v", m)
	}
}
