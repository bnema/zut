package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type agentTimeContext struct {
	started  time.Time
	location *time.Location
	zone     string
	offset   string
}

func newAgentTimeContext(now time.Time) agentTimeContext {
	zoneName, offset := localTimeMetadata(now)
	return agentTimeContext{
		started:  now,
		location: now.Location(),
		zone:     zoneName,
		offset:   offset,
	}
}

func localTimeMetadata(now time.Time) (string, string) {
	zone, seconds := now.Zone()
	zoneName := now.Location().String()
	if zoneName == "" || zoneName == "Local" {
		zoneName = zone
	}
	return zoneName, formatUTCOffset(seconds)
}

func (a *Agent) SetSessionTimeContext(started time.Time, timezone, offset string) {
	if started.IsZero() {
		return
	}
	location := time.Local
	loadedTimezone := false
	if timezone != "" && timezone != "Local" {
		if loaded, err := time.LoadLocation(timezone); err == nil {
			location = loaded
			loadedTimezone = true
		}
	}
	if !loadedTimezone && offset != "" {
		if fixed := parseUTCOffset(offset); fixed != nil {
			location = fixed
		}
	}
	if timezone == "" {
		timezone = location.String()
	}
	if offset == "" {
		_, seconds := started.In(location).Zone()
		offset = formatUTCOffset(seconds)
	}

	a.mu.Lock()
	a.timeContext = agentTimeContext{
		started:  started,
		location: location,
		zone:     timezone,
		offset:   offset,
	}
	a.hasSessionTimeContext = true
	a.mu.Unlock()
}

func (a *Agent) providerTimeContext() agentTimeContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSessionTimeContext {
		return agentTimeContext{}
	}
	return a.timeContext
}

func (c agentTimeContext) systemText() string {
	return c.developerText()
}

// developerText returns host context projected after stable instructions.
// Minute precision is sufficient for coordination and avoids generating a
// unique timestamp solely to distinguish otherwise-equal agents.
func (c agentTimeContext) developerText() string {
	if c.started.IsZero() {
		return ""
	}
	location := c.location
	if location == nil {
		location = time.UTC
	}
	zone := c.zone
	if zone == "" {
		zone = location.String()
	}
	offset := c.offset
	if offset == "" {
		_, seconds := c.started.In(location).Zone()
		offset = formatUTCOffset(seconds)
	}
	return fmt.Sprintf("[Session time context]\nsession_started: %s\nlocal_timezone: %s (UTC%s)", c.started.In(location).Format("2006-01-02 15:04"), zone, offset)
}

func formatUTCOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, (seconds%3600)/60)
}

func parseUTCOffset(value string) *time.Location {
	if len(value) != len("+00:00") || (value[0] != '+' && value[0] != '-') || value[3] != ':' {
		return nil
	}
	hours, errHours := strconv.Atoi(value[1:3])
	minutes, errMinutes := strconv.Atoi(value[4:6])
	if errHours != nil || errMinutes != nil || hours > 23 || minutes > 59 {
		return nil
	}
	seconds := hours*3600 + minutes*60
	if strings.HasPrefix(value, "-") {
		seconds = -seconds
	}
	return time.FixedZone(value, seconds)
}
