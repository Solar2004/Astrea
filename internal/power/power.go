// Package power keeps the controlled machine reachable and reports whether
// someone is using it: a sleep inhibitor (so a PC serving local models does
// not suspend), user-idle detection (so the AI can use the GPU only while
// nobody is at the keyboard), Wake-on-LAN, and power actions.
package power

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Awake modes.
const (
	AwakeOff    = "off"    // let the OS sleep normally
	AwakeAuto   = "auto"   // stay awake while Busy() reports work (e.g. a running service)
	AwakeAlways = "always" // never sleep while the agent runs
)

// Keeper applies an awake mode. Busy is polled in auto mode.
type Keeper struct {
	Busy func() bool

	mu      sync.Mutex
	mode    string
	holding bool
	inhib   sleepInhibitor
}

// sleepInhibitor blocks (on) or allows (off) system sleep.
type sleepInhibitor interface{ set(on bool) error }

// NewKeeper starts a keeper in the given mode ("" = auto).
func NewKeeper(mode string, busy func() bool) (*Keeper, error) {
	k := &Keeper{Busy: busy, inhib: newInhibitor()}
	if err := k.SetMode(mode); err != nil {
		return nil, err
	}
	go func() {
		for range time.Tick(20 * time.Second) {
			k.apply()
		}
	}()
	return k, nil
}

// SetMode switches the awake mode and applies it immediately.
func (k *Keeper) SetMode(mode string) error {
	if mode == "" {
		mode = AwakeAuto
	}
	switch mode {
	case AwakeOff, AwakeAuto, AwakeAlways:
	default:
		return fmt.Errorf("awake mode must be off, auto or always")
	}
	k.mu.Lock()
	k.mode = mode
	k.mu.Unlock()
	k.apply()
	return nil
}

// State reports the mode and whether sleep is currently inhibited.
func (k *Keeper) State() (mode string, holding bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.mode, k.holding
}

func (k *Keeper) apply() {
	k.mu.Lock()
	defer k.mu.Unlock()
	want := k.mode == AwakeAlways || (k.mode == AwakeAuto && k.Busy != nil && k.Busy())
	if want == k.holding {
		return
	}
	if err := k.inhib.set(want); err == nil {
		k.holding = want
	}
}

// Idle describes user activity on the machine.
type Idle struct {
	// IdleSec is the time since the last input made by a person (Astrea's
	// own clicks and keystrokes do not count).
	IdleSec float64 `json:"idle_sec"`
	// Foreground is the focused window title and its process, when known.
	Foreground string `json:"foreground,omitempty"`
	Process    string `json:"process,omitempty"`
	// Locked: the Windows lock screen (or another secure desktop) is up.
	Locked bool `json:"locked,omitempty"`
	// UserActive is true when a person is at the machine: unlocked, and
	// input at two or more moments in the last threshold seconds.
	UserActive bool `json:"user_active"`
	// Why explains the verdict in a few words.
	Why string `json:"why,omitempty"`
}

// presence tells a person at the keyboard from everything else that also
// resets the OS idle timer: Astrea's own input, a display waking up, a desk
// bump. It samples the last-input time every couple of seconds.
type presence struct {
	mu       sync.Mutex
	injected time.Time   // last input injected by Astrea
	last     time.Time   // last input time seen
	human    []time.Time // moments of human input, oldest first
	started  bool
}

var pres presence

const (
	sampleEvery = 2 * time.Second
	sameMoment  = 3 * time.Second // inputs closer than this are one moment
	keepHistory = 10 * time.Minute
)

// MarkInjected records that Astrea just sent mouse or keyboard input, so it
// is not mistaken for a person.
func MarkInjected() {
	pres.mu.Lock()
	pres.injected = time.Now()
	pres.mu.Unlock()
}

// sample folds the current OS idle time into the history of human input.
func (p *presence) sample(now time.Time, idle time.Duration) {
	at := now.Add(-idle)
	if !p.last.IsZero() && at.Sub(p.last).Abs() < 500*time.Millisecond {
		return // nothing new (jitter of the clock reading)
	}
	p.last = at
	if !p.injected.IsZero() && at.Sub(p.injected).Abs() < 1500*time.Millisecond {
		return // our own click or keystroke
	}
	if n := len(p.human); n > 0 && at.Sub(p.human[n-1]) < sameMoment {
		p.human[n-1] = at
	} else {
		p.human = append(p.human, at)
	}
	for len(p.human) > 0 && now.Sub(p.human[0]) > keepHistory {
		p.human = p.human[1:]
	}
}

// verdict: human idle time and whether there were two separate moments of
// input inside the threshold.
func (p *presence) verdict(now time.Time, threshold time.Duration) (idle time.Duration, active bool, why string) {
	n := len(p.human)
	if n == 0 {
		return now.Sub(p.last), false, "no input from a person"
	}
	idle = now.Sub(p.human[n-1])
	if idle >= threshold {
		return idle, false, "no input from a person recently"
	}
	if n >= 2 && now.Sub(p.human[n-2]) < threshold {
		return idle, true, "keyboard/mouse in use"
	}
	return idle, false, "a single touch, not someone using it"
}

func (p *presence) loop() {
	for range time.Tick(sampleEvery) {
		if d, err := idleTime(); err == nil {
			p.mu.Lock()
			p.sample(time.Now(), d)
			p.mu.Unlock()
		}
	}
}

// UserIdle reports whether a person is using the machine. The first call
// starts a background sampler; until it has history, any recent input counts.
func UserIdle(activeThreshold time.Duration) (Idle, error) {
	d, err := idleTime()
	if err != nil {
		return Idle{}, err
	}
	now := time.Now()
	title, proc := foreground()
	out := Idle{Foreground: title, Process: proc}

	pres.mu.Lock()
	fresh := !pres.started
	if fresh {
		pres.started = true
		go pres.loop()
	}
	pres.sample(now, d)
	idle, active, why := pres.verdict(now, activeThreshold)
	pres.mu.Unlock()
	if fresh && d < activeThreshold && len(pres.human) > 0 {
		active, why = true, "recent input (no history yet)"
	}
	out.IdleSec = idle.Seconds()

	if screenLocked(proc) {
		out.Locked, active, why = true, false, "the lock screen is up"
	}
	out.UserActive, out.Why = active, why
	return out, nil
}

// Wake sends a Wake-on-LAN magic packet for mac to the broadcast address
// (default 255.255.255.255:9). It must be sent from a machine on the same LAN
// as the sleeping one, e.g. another agent.
func Wake(mac, broadcast string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return fmt.Errorf("invalid MAC address %q", mac)
	}
	if broadcast == "" {
		broadcast = "255.255.255.255:9"
	} else if !strings.Contains(broadcast, ":") {
		broadcast += ":9"
	}
	pkt := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xff)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, hw...)
	}
	conn, err := net.Dial("udp", broadcast)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(pkt)
	return err
}

// Actions lists the accepted power actions.
var Actions = []string{"lock", "sleep", "hibernate", "reboot", "shutdown", "cancel"}

// ActionCommand returns the argv for a power action after delaySec seconds
// (reboot/shutdown only; "cancel" aborts a pending one).
func ActionCommand(action string, delaySec int) (string, []string, error) {
	d := strconv.Itoa(delaySec)
	if runtime.GOOS == "windows" {
		switch action {
		case "lock":
			return "rundll32.exe", []string{"user32.dll,LockWorkStation"}, nil
		case "sleep":
			return "powershell", []string{"-NoProfile", "-Command", "Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.Application]::SetSuspendState('Suspend', $false, $false)"}, nil
		case "hibernate":
			return "shutdown", []string{"/h"}, nil
		case "reboot":
			return "shutdown", []string{"/r", "/t", d}, nil
		case "shutdown":
			return "shutdown", []string{"/s", "/t", d}, nil
		case "cancel":
			return "shutdown", []string{"/a"}, nil
		}
	} else {
		switch action {
		case "lock":
			return "loginctl", []string{"lock-session"}, nil
		case "sleep":
			return "systemctl", []string{"suspend"}, nil
		case "hibernate":
			return "systemctl", []string{"hibernate"}, nil
		case "reboot":
			return "shutdown", []string{"-r", "+" + strconv.Itoa((delaySec+59)/60)}, nil
		case "shutdown":
			return "shutdown", []string{"-h", "+" + strconv.Itoa((delaySec+59)/60)}, nil
		case "cancel":
			return "shutdown", []string{"-c"}, nil
		}
	}
	return "", nil, fmt.Errorf("unknown action %q (want one of %v)", action, Actions)
}

// Run executes a power action.
func Run(action string, delaySec int) (string, error) {
	name, args, err := ActionCommand(action, delaySec)
	if err != nil {
		return "", err
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
