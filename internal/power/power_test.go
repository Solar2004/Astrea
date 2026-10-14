package power

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestWakeSendsMagicPacket(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if err := Wake("3c:7c:3f:12:34:56", pc.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	mac := []byte{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}
	if n != 102 || !bytes.Equal(buf[:6], bytes.Repeat([]byte{0xff}, 6)) || !bytes.Equal(buf[96:102], mac) {
		t.Fatalf("bad magic packet (%d bytes): %x", n, buf[:n])
	}
	if Wake("not-a-mac", "") == nil {
		t.Fatal("invalid MAC should fail")
	}
}

func TestActionCommandValidation(t *testing.T) {
	for _, a := range Actions {
		if _, _, err := ActionCommand(a, 5); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, _, err := ActionCommand("format-c", 0); err == nil {
		t.Fatal("unknown action should fail")
	}
}

type fakeInhib struct{ on *bool }

func (f fakeInhib) set(on bool) error { *f.on = on; return nil }

func TestKeeperAutoFollowsBusy(t *testing.T) {
	busy, on := false, false
	k := &Keeper{Busy: func() bool { return busy }}
	k.inhib = fakeInhib{&on}
	_ = k.SetMode(AwakeAuto)
	if on {
		t.Fatal("idle machine must be allowed to sleep")
	}
	busy = true
	k.apply()
	if !on {
		t.Fatal("busy machine must stay awake")
	}
	_ = k.SetMode(AwakeOff)
	if on {
		t.Fatal("off must release the inhibitor")
	}
	if k.SetMode("nope") == nil {
		t.Fatal("invalid mode should fail")
	}
}
