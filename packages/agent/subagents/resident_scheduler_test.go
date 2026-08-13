package subagents

import "testing"

func TestResidentSchedulerUsesSixSlotDefault(t *testing.T) {
	if got := NewResidentScheduler(0).limit; got != DefaultResidentConcurrency {
		t.Fatalf("default limit = %d, want %d", got, DefaultResidentConcurrency)
	}
}

func TestResidentSchedulerNormalizesNegativeLimit(t *testing.T) {
	if got := NewResidentScheduler(-1).limit; got != DefaultResidentConcurrency {
		t.Fatalf("negative limit = %d, want default %d", got, DefaultResidentConcurrency)
	}
}

func TestResidentSchedulerSelectsOldestEligibleWithoutBlockingOtherChildren(t *testing.T) {
	scheduler := NewResidentScheduler(2)
	first := scheduler.Enqueue("child-a", "first")
	second := scheduler.Enqueue("child-a", "second")
	third := scheduler.Enqueue("child-b", "third")

	if got, ok := scheduler.Admit(); !ok || got.Sequence != first.Sequence {
		t.Fatalf("first admit = %#v, want %#v", got, first)
	}
	if got, ok := scheduler.Admit(); !ok || got.Sequence != third.Sequence {
		t.Fatalf("second admit = %#v, want child-b ticket %#v", got, third)
	}
	scheduler.Release("child-a")
	if got, ok := scheduler.Admit(); !ok || got.Sequence != second.Sequence {
		t.Fatalf("third admit = %#v, want queued child-a ticket %#v", got, second)
	}
}
