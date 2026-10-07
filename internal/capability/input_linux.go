//go:build linux

package capability

import (
	"fmt"
	"os/exec"
	"strconv"
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
