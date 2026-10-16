//go:build !windows

package uptime

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// osSpans reads past boots from the systemd journal (one span per boot; the
// journal does not say how each ended).
func osSpans(ctx context.Context, since time.Time) []Span {
	out, err := exec.CommandContext(ctx, "journalctl", "--list-boots", "-o", "json", "-q").Output()
	if err != nil {
		return nil
	}
	var boots []struct {
		First int64 `json:"first_entry"`
		Last  int64 `json:"last_entry"`
	}
	if json.Unmarshal(out, &boots) != nil {
		return nil
	}
	var spans []Span
	for i, b := range boots {
		if i == len(boots)-1 || b.First == 0 {
			continue // the current boot is the agent's own
		}
		sp := Span{Start: time.UnixMicro(b.First).UTC(), End: time.UnixMicro(b.Last).UTC(), EndKind: EndOff}
		if sp.End.After(since) {
			spans = append(spans, sp)
		}
	}
	return spans
}

func endKindAfter(time.Time) string { return EndOff }
