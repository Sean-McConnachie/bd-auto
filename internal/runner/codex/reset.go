package codex

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const resetGrace = time.Hour

var (
	resetRelative = regexp.MustCompile(`(?i)\b(?:resets?|try again)\s+in\s+(\d+)\s*(second|sec|minute|min|hour|hr|day)s?\b`)
	resetClock    = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?,?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([A-Za-z0-9_+:/-]+)\))?`)
	resetISO      = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2}))`)
)

func parseReset(text string, now time.Time) (time.Time, bool) {
	if match := resetRelative.FindStringSubmatch(text); match != nil {
		n, err := strconv.Atoi(match[1])
		if err == nil {
			return now.Add(time.Duration(n) * resetUnit(match[2])), true
		}
	}
	if match := resetISO.FindStringSubmatch(text); match != nil {
		return parseAbsoluteReset(match[1], now)
	}
	match := resetClock.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, false
	}
	hour, ok := clockHour(match[2], match[4])
	if !ok {
		return time.Time{}, false
	}
	minute := 0
	if match[3] != "" {
		minute, _ = strconv.Atoi(match[3])
		if minute > 59 {
			return time.Time{}, false
		}
	} else if match[4] == "" {
		return time.Time{}, false
	}
	loc := now.Location()
	if match[5] != "" {
		var ok bool
		loc, ok = resetLocation(match[5])
		if !ok {
			return time.Time{}, false
		}
	}
	local := now.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if weekday, ok := parseWeekday(match[1]); ok {
		for i := 0; i < 7 && at.Weekday() != weekday; i++ {
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

func resetLocation(value string) (*time.Location, bool) {
	if loaded, err := time.LoadLocation(value); err == nil {
		return loaded, true
	}
	if len(value) == 6 && (value[0] == '+' || value[0] == '-') && value[3] == ':' {
		hours, hourErr := strconv.Atoi(value[1:3])
		minutes, minuteErr := strconv.Atoi(value[4:6])
		if hourErr == nil && minuteErr == nil && hours <= 23 && minutes <= 59 {
			offset := (hours*60 + minutes) * 60
			if value[0] == '-' {
				offset = -offset
			}
			return time.FixedZone(value, offset), true
		}
	}
	offsets := map[string]int{
		"UTC": 0, "GMT": 0,
		"EST": -5, "EDT": -4,
		"CST": -6, "CDT": -5,
		"MST": -7, "MDT": -6,
		"PST": -8, "PDT": -7,
		"CET": 1, "CEST": 2,
	}
	name := strings.ToUpper(value)
	if hours, ok := offsets[name]; ok {
		return time.FixedZone(name, hours*60*60), true
	}
	return nil, false
}

func parseAbsoluteReset(value string, _ time.Time) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if at, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

func resetUnit(unit string) time.Duration {
	switch strings.ToLower(unit) {
	case "second", "sec":
		return time.Second
	case "hour", "hr":
		return time.Hour
	case "day":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

func clockHour(raw, meridiem string) (int, bool) {
	hour, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(meridiem) {
	case "am":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour == 12 {
			return 0, true
		}
	case "pm":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour != 12 {
			hour += 12
		}
	default:
		if hour > 23 {
			return 0, false
		}
	}
	return hour, true
}

func parseWeekday(value string) (time.Weekday, bool) {
	weekdays := map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
	day, ok := weekdays[strings.ToLower(value)]
	return day, ok
}
