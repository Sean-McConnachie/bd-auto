package claude

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A plan limit is the one outage that knows when it ends, and the CLI prints it
// in the result line it already sends:
//
//	"You've hit your session limit · resets 3:20pm (Europe/Berlin)"
//
// Nothing read it, so a drain that met a wall with tens of minutes left on it
// retried five times over 75 seconds and then parked the issues. Backing off
// cannot help against a limit; the only two useful answers are to wait for it
// or to stop and say when to come back, and both need this number.
//
// This is prose, which class.go argues at length is a losing game to classify
// on — and that argument still holds. What is different here is the cost of
// being wrong. Classification decides whether an issue is parked; this decides
// only how long to wait and what the stop reason says, and wording it does not
// recognise leaves the reset unset, which is exactly the behaviour every caller
// had before it existed.

// resetGrace is how far into the past a reset time is still read as the one
// just reported rather than as tomorrow's.
//
// The process that met the limit takes time to die and the engine takes time to
// read it, so a clock that has just gone past is the ordinary case. Reading
// 15:20 at 15:22 as tomorrow's 15:20 would turn a wait of nothing into a stop
// with a day on it.
const resetGrace = time.Hour

// resetClock reads "resets 3:20pm (Europe/Berlin)" and its near neighbours:
// "resets at 15:20", "resets 9am", and the weekly limit's "resets Monday 9am".
//
// Minutes or a meridiem must be there. A bare "resets 15" is as likely to be a
// count of something as a clock, and a wrong hour here is worse than no hour.
var resetClock = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?` +
	`(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?,?\s+)?` +
	`(\d{1,2})(?::(\d{2}))?\s*(am|pm)?` +
	`(?:\s*\(([A-Za-z0-9_+/-]+)\))?`)

// resetIn reads the relative form, "resets in 26 minutes".
var resetIn = regexp.MustCompile(`(?i)\bresets?\s+in\s+(\d+)\s*(second|sec|minute|min|hour|hr|day)s?\b`)

// resetAt reads a limit's reset time out of what the CLI said, taking the first
// of the texts that carries one. Zero and false where none does.
func resetAt(now time.Time, texts ...string) (time.Time, bool) {
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if at, ok := parseReset(t, now); ok {
			return at, true
		}
	}
	return time.Time{}, false
}

// parseReset reads one text.
func parseReset(text string, now time.Time) (time.Time, bool) {
	if m := resetIn.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, false
		}
		return now.Add(time.Duration(n) * resetUnit(m[2])), true
	}

	m := resetClock.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, ok := clockHour(m[2], m[4])
	if !ok {
		return time.Time{}, false
	}
	min := 0
	if m[3] != "" {
		min, _ = strconv.Atoi(m[3])
		if min > 59 {
			return time.Time{}, false
		}
	} else if m[4] == "" {
		// Neither minutes nor a meridiem: not a clock time.
		return time.Time{}, false
	}

	// The zone the CLI names is the account holder's, which is normally this
	// machine's too. Where it cannot be loaded — an abbreviation, a host with no
	// tzdata — local is the better guess than nothing: it is right whenever the
	// two agree, and the engine only ever spends a bounded wait on it.
	loc := now.Location()
	if zone := m[5]; zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}

	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if wd, ok := weekday(m[1]); ok {
		for i := 0; i < 7 && at.Weekday() != wd; i++ {
			at = at.AddDate(0, 0, 1)
		}
		if at.Before(local.Add(-resetGrace)) {
			at = at.AddDate(0, 0, 7)
		}
		return at, true
	}
	if at.Before(local.Add(-resetGrace)) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

// clockHour turns the hour and its meridiem into a 24 hour clock.
func clockHour(raw, meridiem string) (int, bool) {
	h, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(meridiem) {
	case "am":
		if h < 1 || h > 12 {
			return 0, false
		}
		if h == 12 {
			return 0, true
		}
		return h, true
	case "pm":
		if h < 1 || h > 12 {
			return 0, false
		}
		if h == 12 {
			return 12, true
		}
		return h + 12, true
	}
	if h > 23 {
		return 0, false
	}
	return h, true
}

// weekday maps the three-letter prefix the pattern captured onto a day.
func weekday(prefix string) (time.Weekday, bool) {
	switch strings.ToLower(prefix) {
	case "sun":
		return time.Sunday, true
	case "mon":
		return time.Monday, true
	case "tue":
		return time.Tuesday, true
	case "wed":
		return time.Wednesday, true
	case "thu":
		return time.Thursday, true
	case "fri":
		return time.Friday, true
	case "sat":
		return time.Saturday, true
	}
	return 0, false
}

// resetUnit maps the unit resetIn captured onto a duration.
func resetUnit(unit string) time.Duration {
	switch strings.ToLower(unit) {
	case "second", "sec":
		return time.Second
	case "hour", "hr":
		return time.Hour
	case "day":
		return 24 * time.Hour
	}
	return time.Minute
}
