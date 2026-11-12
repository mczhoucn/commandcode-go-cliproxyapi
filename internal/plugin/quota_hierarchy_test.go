package plugin

import (
	"strings"
	"testing"
)

// month, week and five-hour nest: the shorter window is carved out of the
// longer one. When a longer window is spent the shorter ones must read as
// spent too, even though the upstream reports them independently and may send
// an idle value. Rendering the shorter window as available would invite the
// user into requests the account cannot serve.
func TestWindowHierarchyPropagatesDownward(t *testing.T) {
	t.Run("spent month zeroes week and five-hour", func(t *testing.T) {
		u := &quotaUsage{
			Month:    &quotaWindow{Status: "exceeded", Used: 70, Cap: 70},
			Weekly:   quotaWindow{Status: "ok", Used: 1, Cap: 35},
			FiveHour: quotaWindow{Status: "ok", Used: 0, Cap: 14},
		}
		applyWindowHierarchy(u)
		if u.Weekly.Status != "exceeded" || u.Weekly.Percent != 100 {
			t.Errorf("weekly = %+v, want exhausted", u.Weekly)
		}
		if u.FiveHour.Status != "exceeded" || u.FiveHour.Percent != 100 {
			t.Errorf("five-hour = %+v, want exhausted", u.FiveHour)
		}
		if u.Weekly.Cap != 35 || u.FiveHour.Cap != 14 {
			t.Errorf("caps must be preserved for the denominator: %+v %+v", u.Weekly, u.FiveHour)
		}
	})

	t.Run("spent week zeroes only five-hour", func(t *testing.T) {
		month := &quotaWindow{Status: "ok", Used: 10, Cap: 70}
		u := &quotaUsage{
			Month:    month,
			Weekly:   quotaWindow{Status: "exceeded", Used: 35, Cap: 35},
			FiveHour: quotaWindow{Status: "ok", Used: 0, Cap: 14},
		}
		applyWindowHierarchy(u)
		if u.FiveHour.Status != "exceeded" {
			t.Errorf("five-hour = %+v, want exhausted", u.FiveHour)
		}
		// Propagation is downward only: the month's own headroom is untouched.
		if u.Month.Status != "ok" || u.Month.Used != 10 {
			t.Errorf("month must not be altered by a spent week: %+v", u.Month)
		}
	})

	t.Run("spent five-hour leaves longer windows alone", func(t *testing.T) {
		u := &quotaUsage{
			Weekly:   quotaWindow{Status: "ok", Used: 5, Cap: 35},
			FiveHour: quotaWindow{Status: "exceeded", Used: 14, Cap: 14},
		}
		applyWindowHierarchy(u)
		if u.Weekly.Status != "ok" || u.Weekly.Used != 5 {
			t.Errorf("weekly must keep its own value: %+v", u.Weekly)
		}
	})

	t.Run("healthy snapshot unchanged", func(t *testing.T) {
		u := &quotaUsage{
			Month:    &quotaWindow{Status: "ok", Used: 5, Cap: 70},
			Weekly:   quotaWindow{Status: "ok", Used: 5, Cap: 35},
			FiveHour: quotaWindow{Status: "ok", Used: 1, Cap: 14},
		}
		before := *u
		applyWindowHierarchy(u)
		if u.Weekly != before.Weekly || u.FiveHour != before.FiveHour {
			t.Errorf("healthy windows must not change: %+v", u)
		}
	})
}

// An "unlimited" window has no cap to exhaust, so a 100%-used unlimited window
// must not be treated as exhausted — that would zero everything below it.
func TestUnlimitedWindowIsNeverExhausted(t *testing.T) {
	unlimited := quotaWindow{Status: "unlimited", Used: 100, Cap: 0}
	if unlimited.exhausted() {
		t.Fatal("an unlimited window must never count as exhausted")
	}
	u := &quotaUsage{Weekly: unlimited, FiveHour: quotaWindow{Status: "ok", Used: 1, Cap: 14}}
	applyWindowHierarchy(u)
	if u.FiveHour.Status != "ok" {
		t.Errorf("unlimited week must not zero the five-hour window: %+v", u.FiveHour)
	}
}

// Usage at or above the cap is exhausted even when the upstream did not flag
// it: CommandCode's reported used can round past the cap.
func TestUsedAtCapCountsAsExhausted(t *testing.T) {
	atCap := quotaWindow{Status: "ok", Used: 35, Cap: 35}
	if !atCap.exhausted() {
		t.Error("used == cap must count as exhausted")
	}
	overCap := quotaWindow{Status: "ok", Used: 35.5, Cap: 35}
	if !overCap.exhausted() {
		t.Error("used > cap must count as exhausted")
	}
	// An unknown cap imposes no limit.
	noCap := quotaWindow{Status: "ok", Used: 100, Cap: 0}
	if noCap.exhausted() {
		t.Error("a zero cap must not count as exhausted")
	}
}

// The reported window name drives the operator-facing message, and the longest
// spent window wins so the message names the limit that actually blocks.
func TestExhaustedWindowPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		usage quotaUsage
		want  string
	}{
		{
			name:  "month wins",
			usage: quotaUsage{Month: &quotaWindow{Status: "exceeded", Used: 70, Cap: 70}, Weekly: quotaWindow{Status: "exceeded", Used: 35, Cap: 35}},
			want:  "month",
		},
		{
			name:  "weekly next",
			usage: quotaUsage{Month: &quotaWindow{Status: "ok", Used: 1, Cap: 70}, Weekly: quotaWindow{Status: "exceeded", Used: 35, Cap: 35}},
			want:  "weekly",
		},
		{
			name:  "five-hour last",
			usage: quotaUsage{Weekly: quotaWindow{Status: "ok", Used: 1, Cap: 35}, FiveHour: quotaWindow{Status: "exceeded", Used: 14, Cap: 14}},
			want:  "five_hour",
		},
		{
			name:  "healthy",
			usage: quotaUsage{Weekly: quotaWindow{Status: "ok", Used: 1, Cap: 35}, FiveHour: quotaWindow{Status: "ok", Used: 1, Cap: 14}},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.exhaustedWindow(); got != tc.want {
				t.Errorf("exhaustedWindow() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A spent credential must make the standard quota fetch fail, naming the
// window, rather than returning a 0% card that reads as healthy-but-idle.
func TestQuotaFetchFailsWhenWindowExhausted(t *testing.T) {
	for _, window := range []string{"month", "weekly", "five_hour"} {
		err := quotaExhaustedError(window)
		if err == nil {
			t.Fatalf("%s: nil error", window)
		}
		msg := err.Error()
		switch window {
		case "month":
			if !strings.Contains(msg, "monthly") {
				t.Errorf("month message = %q", msg)
			}
		case "weekly":
			if !strings.Contains(msg, "weekly") {
				t.Errorf("weekly message = %q", msg)
			}
		case "five_hour":
			if !strings.Contains(msg, "5-hour") {
				t.Errorf("five-hour message = %q", msg)
			}
		}
	}
}
