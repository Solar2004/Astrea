//go:build linux

package capability

import (
	"bytes"
	"hash/fnv"
	"os"
	"os/exec"
	"strings"
)

func init() {
	// Wayland: wl-clipboard. X11: xclip. Neither: no clipboard.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			clip = cmdClip{get: []string{"wl-paste", "--no-newline"}, set: []string{"wl-copy"},
				types: []string{"wl-paste", "--list-types"}}
			return
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		clip = cmdClip{get: []string{"xclip", "-selection", "clipboard", "-o"}, set: []string{"xclip", "-selection", "clipboard", "-i"},
			types: []string{"xclip", "-selection", "clipboard", "-o", "-t", "TARGETS"}}
	}
}

// cmdClip drives the clipboard through wl-clipboard or xclip.
type cmdClip struct{ get, set, types []string }

func (c cmdClip) Text() (string, bool, error) {
	types, _ := exec.Command(c.types[0], c.types[1:]...).Output()
	t := string(types)
	if !strings.Contains(t, "text") && !strings.Contains(t, "STRING") && strings.Contains(t, "image/") {
		return "", true, nil
	}
	out, err := exec.Command(c.get[0], c.get[1:]...).Output()
	if err != nil {
		return "", false, nil // empty clipboard exits non-zero
	}
	return string(out), false, nil
}

func (c cmdClip) SetText(text string) error {
	cmd := exec.Command(c.set[0], c.set[1:]...)
	cmd.Stdin = strings.NewReader(text)
	// wl-copy/xclip keep serving the selection in the background: do not wait.
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// Seq hashes the content: these tools have no change counter.
func (c cmdClip) Seq() uint64 {
	out, _ := exec.Command(c.get[0], c.get[1:]...).Output()
	types, _ := exec.Command(c.types[0], c.types[1:]...).Output()
	h := fnv.New64a()
	h.Write(out)
	h.Write(bytes.TrimSpace(types))
	return h.Sum64()
}
