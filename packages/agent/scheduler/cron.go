// Package scheduler provides portable, in-process calendar scheduling.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a five-field cron schedule (minute, hour, day of month, month, day
// of week). It evaluates calendar times in the location captured when the
// schedule is created. A repeated local minute during a DST fall-back matches
// twice; a local minute skipped during a spring-forward does not match.
type Cron struct {
	minute field
	hour   field
	day    field
	month  field
	week   field
	loc    *time.Location
}

type field struct {
	values   []bool
	wildcard bool
}

var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var weekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseCron parses a conventional five-field cron expression. Month and
// weekday names use the English three-letter abbreviations. The supplied
// location is retained by the schedule, so later changes to time.Local do not
// change an existing task's calendar semantics.
func ParseCron(expression string, location *time.Location) (Cron, error) {
	if location == nil {
		return Cron{}, fmt.Errorf("cron timezone is required")
	}
	expression = strings.ToLower(strings.TrimSpace(expression))
	if replacement, ok := cronMacros[expression]; ok {
		expression = replacement
	}
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("cron requires five fields (minute hour day-of-month month day-of-week)")
	}
	minute, err := parseField(parts[0], 0, 59, nil, false)
	if err != nil {
		return Cron{}, fmt.Errorf("invalid cron minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23, nil, false)
	if err != nil {
		return Cron{}, fmt.Errorf("invalid cron hour: %w", err)
	}
	day, err := parseField(parts[2], 1, 31, nil, false)
	if err != nil {
		return Cron{}, fmt.Errorf("invalid cron day-of-month: %w", err)
	}
	month, err := parseField(parts[3], 1, 12, monthNames, false)
	if err != nil {
		return Cron{}, fmt.Errorf("invalid cron month: %w", err)
	}
	week, err := parseField(parts[4], 0, 7, weekdayNames, true)
	if err != nil {
		return Cron{}, fmt.Errorf("invalid cron day-of-week: %w", err)
	}
	return Cron{minute: minute, hour: hour, day: day, month: month, week: week, loc: location}, nil
}

// Next returns the first schedule occurrence strictly after after. An invalid
// calendar expression such as 31 February has no occurrence and returns the
// zero time.
func (c Cron) Next(after time.Time) time.Time {
	if c.loc == nil {
		return time.Time{}
	}
	candidate := after.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	// Eight years covers every leap-year placement while keeping impossible
	// expressions bounded. Normal schedules resolve in a handful of steps.
	const maxMinutes = 8 * 366 * 24 * 60
	for range maxMinutes {
		if c.matches(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}
}

func (c Cron) matches(t time.Time) bool {
	if !c.minute.matches(t.Minute()) || !c.hour.matches(t.Hour()) ||
		!c.month.matches(int(t.Month())) {
		return false
	}
	dom := c.day.matches(t.Day())
	dow := c.week.matches(int(t.Weekday()))
	// Traditional cron treats restricted day-of-month and day-of-week fields
	// as an OR. When either is *, the other field controls matching.
	if !c.day.wildcard && !c.week.wildcard {
		return dom || dow
	}
	return dom && dow
}

func (f field) matches(value int) bool {
	return value >= 0 && value < len(f.values) && f.values[value]
}

func parseField(raw string, min, max int, aliases map[string]int, sundaySeven bool) (field, error) {
	out := field{values: make([]bool, max+1), wildcard: raw == "*"}
	if raw == "" {
		return field{}, fmt.Errorf("empty field")
	}
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			return field{}, fmt.Errorf("empty list item")
		}
		base, step, hasStep, err := splitStep(part)
		if err != nil {
			return field{}, err
		}
		start, end := min, max
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			pieces := strings.Split(base, "-")
			if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
				return field{}, fmt.Errorf("invalid range %q", base)
			}
			start, err = parseCronValue(pieces[0], min, max, aliases, sundaySeven)
			if err != nil {
				return field{}, err
			}
			end, err = parseCronValue(pieces[1], min, max, aliases, sundaySeven)
			if err != nil {
				return field{}, err
			}
			if start > end {
				return field{}, fmt.Errorf("range %q descends", base)
			}
		default:
			if hasStep {
				return field{}, fmt.Errorf("step requires * or a range")
			}
			start, err = parseCronValue(base, min, max, aliases, sundaySeven)
			if err != nil {
				return field{}, err
			}
			end = start
		}
		if !hasStep {
			step = 1
		}
		for value := start; value <= end; value += step {
			out.values[value] = true
		}
	}
	return out, nil
}

func splitStep(part string) (base string, step int, hasStep bool, err error) {
	pieces := strings.Split(part, "/")
	if len(pieces) == 1 {
		return part, 0, false, nil
	}
	if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
		return "", 0, false, fmt.Errorf("invalid step %q", part)
	}
	step, err = strconv.Atoi(pieces[1])
	if err != nil || step <= 0 {
		return "", 0, false, fmt.Errorf("invalid step %q", part)
	}
	return pieces[0], step, true, nil
}

func parseCronValue(raw string, min, max int, aliases map[string]int, sundaySeven bool) (int, error) {
	if value, ok := aliases[raw]; ok {
		return value, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	if sundaySeven && value == 7 {
		return 0, nil
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%d is outside %d-%d", value, min, max)
	}
	return value, nil
}
