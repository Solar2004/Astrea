//go:build linux

package capability

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func init() {
	// xdotool works on X11. On Wayland the user needs ydotool instead; we keep
	// the backend nil in that case so callers get a clear "not supported".
	if _, err := exec.LookPath("xdotool"); err == nil {
		input = linuxInput{}
	}
}

// linuxInput drives input through xdotool (X11).
type linuxInput struct{}

func (linuxInput) MouseMove(x, y int) error {
	return run("xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y))
}

func (linuxInput) MouseClick(button string, double bool) error {
	n := "1"
	switch button {
	case "right":
		n = "3"
	case "middle":
		n = "2"
	}
	if double {
		return run("xdotool", "click", "--repeat", "2", n)
	}
	return run("xdotool", "click", n)
}

func (linuxInput) KeyType(text string) error {
	return run("xdotool", "type", "--clearmodifiers", text)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, out)
	}
	return nil
}

func (linuxInput) MouseButton(button string, down bool) error {
	n := map[string]string{"right": "3", "middle": "2"}[button]
	if n == "" {
		n = "1"
	}
	if down {
		return run("xdotool", "mousedown", n)
	}
	return run("xdotool", "mouseup", n)
}

func (linuxInput) Scroll(dx, dy int) error {
	click := func(btn string, n int) error {
		if n == 0 {
			return nil
		}
		return run("xdotool", "click", "--repeat", strconv.Itoa(n), btn)
	}
	up, right := "5", "7"
	if dy < 0 {
		up, dy = "4", -dy
	}
	if dx < 0 {
		right, dx = "6", -dx
	}
	if err := click(up, dy); err != nil {
		return err
	}
	return click(right, dx)
}

// xdoKeys maps key.press names to X keysyms.
var xdoKeys = map[string]string{"ctrl": "ctrl", "shift": "shift", "alt": "alt", "win": "super",
	"enter": "Return", "esc": "Escape", "tab": "Tab", "backspace": "BackSpace", "space": "space",
	"delete": "Delete", "insert": "Insert", "home": "Home", "end": "End", "pageup": "Prior",
	"pagedown": "Next", "left": "Left", "up": "Up", "right": "Right", "down": "Down",
	"capslock": "Caps_Lock", "printscreen": "Print", "menu": "Menu"}

func (linuxInput) KeyPress(keys []string) error {
	syms := make([]string, len(keys))
	for i, k := range keys {
		if s, ok := xdoKeys[k]; ok {
			k = s
		} else if len(k) > 1 { // f1…f12
			k = strings.ToUpper(k)
		}
		syms[i] = k
	}
	return run("xdotool", "key", "--clearmodifiers", strings.Join(syms, "+"))
}
