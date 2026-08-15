package scheduler

import (
	"testing"
	"time"
)

func TestParseCronNext(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*60*60)
	schedule, err := ParseCron("*/15 9-10 * * 1-5", loc)
	if err != nil {
		t.Fatalf("ParseCron() error = %v", err)
	}

	after := time.Date(2026, time.August, 14, 9, 14, 45, 0, loc) // Friday
	want := time.Date(2026, time.August, 14, 9, 15, 0, 0, loc)
	if got := schedule.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestParseCronDayOfMonthOrDayOfWeek(t *testing.T) {
	loc := time.UTC
	schedule, err := ParseCron("0 9 15 * 1", loc)
	if err != nil {
		t.Fatalf("ParseCron() error = %v", err)
	}

	// Vixie cron semantics: when both fields are restricted, either match is
	// sufficient. Monday 2026-06-08 is not the fifteenth.
	after := time.Date(2026, time.June, 7, 9, 0, 0, 0, loc)
	want := time.Date(2026, time.June, 8, 9, 0, 0, 0, loc)
	if got := schedule.Next(after); !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}

func TestParseCronDSTTransitions(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	fallBack, err := ParseCron("30 1 * * *", loc)
	if err != nil {
		t.Fatalf("ParseCron() error = %v", err)
	}
	first := fallBack.Next(time.Date(2026, time.November, 1, 0, 59, 0, 0, loc))
	second := fallBack.Next(first)
	if first.Hour() != 1 || first.Minute() != 30 || second.Hour() != 1 || second.Minute() != 30 || first.Equal(second) || second.Sub(first) != time.Hour {
		t.Fatalf("fall-back occurrences = %v, %v; want 01:30 exactly one elapsed hour apart", first, second)
	}

	springForward, err := ParseCron("30 2 * * *", loc)
	if err != nil {
		t.Fatalf("ParseCron() error = %v", err)
	}
	got := springForward.Next(time.Date(2026, time.March, 8, 1, 59, 0, 0, loc))
	want := time.Date(2026, time.March, 9, 2, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("spring-forward Next() = %v, want %v", got, want)
	}
}

func TestParseCronRejectsInvalidField(t *testing.T) {
	if _, err := ParseCron("61 * * * *", time.UTC); err == nil {
		t.Fatal("ParseCron() error = nil, want invalid minute error")
	}
}

func TestParseCronSupportsMacro(t *testing.T) {
	schedule, err := ParseCron("@daily", time.UTC)
	if err != nil {
		t.Fatalf("ParseCron() error = %v", err)
	}
	got := schedule.Next(time.Date(2026, time.August, 14, 23, 59, 0, 0, time.UTC))
	want := time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next() = %v, want %v", got, want)
	}
}
