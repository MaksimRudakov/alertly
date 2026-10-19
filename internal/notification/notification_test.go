package notification

import (
	"strings"
	"testing"
	"time"
)

func TestStatusLine_ResolvedDuration(t *testing.T) {
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		end  time.Time
		want string
	}{
		{start.Add(45 * time.Second), "Resolved after 45s"},
		{start.Add(12 * time.Minute), "Resolved after 12m"},
		{start.Add(3*time.Hour + 5*time.Minute), "Resolved after 3h5m"},
		{start.Add(2 * time.Hour), "Resolved after 2h"},
		{start.Add(52 * time.Hour), "Resolved after 2d4h"},
		{start.Add(-time.Minute), "Resolved"},
	}
	for _, c := range cases {
		n := Notification{Status: "resolved", StartsAt: start, Timestamp: c.end}
		if got := strings.Join(n.StatusLine(), " · "); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
	if got := (Notification{Status: "resolved", Timestamp: start}).StatusLine()[0]; got != "Resolved" {
		t.Errorf("unknown start: %q", got)
	}
	if got := (Notification{Status: "firing", StartsAt: start, Timestamp: start}).StatusLine()[0]; got != "Firing" {
		t.Errorf("firing: %q", got)
	}
}
