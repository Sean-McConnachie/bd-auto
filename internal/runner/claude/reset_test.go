package claude

import (
	"testing"
	"time"
)

// berlin is the zone the CLI names in the line this whole file exists for.
func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata for Europe/Berlin: %v", err)
	}
	return loc
}

// The wording is a product's, so the cases that matter are the ones a product
// actually printed, plus the ones where reading it wrong would cost something:
// a clock that has just passed, a weekly limit days out, and a number after
// "resets" that is not a time at all.
func TestParseReset(t *testing.T) {
	loc := berlin(t)
	// A Tuesday, well before the limit the drain met.
	now := time.Date(2026, 8, 18, 12, 38, 0, 0, loc)

	cases := []struct {
		name string
		text string
		want time.Time
		ok   bool
	}{
		{
			// Verbatim from .beads/auto/logs/beads-auto-imp-pzi-a1-r0-worker.jsonl.
			name: "the line the drain met",
			text: "You've hit your session limit · resets 3:20pm (Europe/Berlin)",
			want: time.Date(2026, 8, 18, 15, 20, 0, 0, loc),
			ok:   true,
		},
		{
			name: "24 hour clock and no zone",
			text: "You've hit your session limit · resets at 15:20",
			want: time.Date(2026, 8, 18, 15, 20, 0, 0, loc),
			ok:   true,
		},
		{
			name: "an hour with no minutes",
			text: "resets 9pm",
			want: time.Date(2026, 8, 18, 21, 0, 0, 0, loc),
			ok:   true,
		},
		{
			name: "midnight and noon are not the same hour",
			text: "resets 12am",
			// Already past by more than the grace, so it is tomorrow's.
			want: time.Date(2026, 8, 19, 0, 0, 0, 0, loc),
			ok:   true,
		},
		{
			// The whole point of reading it: a weekly limit is days out, and a
			// run that treats it as this afternoon waits for nothing.
			name: "a weekly limit names its day",
			text: "You've hit your weekly limit · resets Monday 9am",
			want: time.Date(2026, 8, 24, 9, 0, 0, 0, loc),
			ok:   true,
		},
		{
			name: "the relative form",
			text: "resets in 26 minutes",
			want: now.Add(26 * time.Minute),
			ok:   true,
		},
		{
			// The process that met the limit takes time to die. A clock that
			// has just gone past is the one just reported, not tomorrow's.
			name: "a reset that has just passed is not tomorrow",
			text: "resets 12:30pm",
			want: time.Date(2026, 8, 18, 12, 30, 0, 0, loc),
			ok:   true,
		},
		{
			name: "a reset long past is tomorrow",
			text: "resets 6am",
			want: time.Date(2026, 8, 19, 6, 0, 0, 0, loc),
			ok:   true,
		},
		{
			// A wrong hour is worse than no hour, so anything that is not
			// clearly a clock is left alone.
			name: "a number that is not a clock",
			text: "the resets 429 errors seen so far",
			ok:   false,
		},
		{
			name: "an hour that is not an hour",
			text: "resets 25:00",
			ok:   false,
		},
		{
			name: "nothing about a reset at all",
			text: "You've hit your session limit",
			ok:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseReset(c.text, now)
			if ok != c.ok {
				t.Fatalf("parseReset(%q) ok = %v, want %v (got %s)", c.text, ok, c.ok, got)
			}
			if !ok {
				return
			}
			if !got.Equal(c.want) {
				t.Errorf("parseReset(%q) = %s, want %s", c.text, got, c.want)
			}
		})
	}
}

// The zone the CLI names is the one that counts. The same clock read in the
// wrong zone is hours out, and hours is the difference between a wait the
// engine sits through and a stop it hands back to a human.
func TestParseResetHonoursTheZoneTheCLINamed(t *testing.T) {
	loc := berlin(t)
	utc := time.Date(2026, 8, 18, 10, 38, 0, 0, time.UTC) // 12:38 in Berlin

	got, ok := parseReset("resets 3:20pm (Europe/Berlin)", utc)
	if !ok {
		t.Fatal("the reset was not read at all")
	}
	if want := time.Date(2026, 8, 18, 15, 20, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
	if d := got.Sub(utc); d != 162*time.Minute {
		t.Errorf("the limit is %s out, want 2h42m: the zone was not applied", d)
	}
}

// An unloadable zone must not throw the reset away. Local is right whenever the
// two agree, which is the ordinary case, and the engine only ever spends a
// bounded wait on being wrong.
func TestParseResetFallsBackToLocalForAnUnknownZone(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 38, 0, 0, time.UTC)
	got, ok := parseReset("resets 3:20pm (Middle-earth/Bree)", now)
	if !ok {
		t.Fatal("an unrecognised zone threw the reset away")
	}
	if want := time.Date(2026, 8, 18, 15, 20, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
}

// resetAt takes the first text that carries one, so the result line wins over
// whatever the process last wrote to stderr.
func TestResetAtTakesTheFirstTextThatSaysSo(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 38, 0, 0, time.UTC)
	got, ok := resetAt(now, "", "You've hit your session limit · resets 3:20pm", "resets 9pm")
	if !ok {
		t.Fatal("nothing was read")
	}
	if want := time.Date(2026, 8, 18, 15, 20, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
	if _, ok := resetAt(now, "", "  "); ok {
		t.Error("empty texts reported a reset")
	}
}
