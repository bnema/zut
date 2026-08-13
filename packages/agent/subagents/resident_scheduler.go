package subagents

import (
	"errors"
	"sync"
)

const DefaultResidentConcurrency = 6

// ResidentTicket is an accepted prompt's stable global admission position.
type ResidentTicket struct {
	Sequence uint64
	ChildID  string
	Prompt   string
	ready    chan struct{}
}

// ResidentScheduler provides fair global admission while enforcing one active
// turn per child. It intentionally does not perform provider work.
type ResidentScheduler struct {
	mu      sync.Mutex
	limit   int
	next    uint64
	active  int
	running map[string]bool
	pending []ResidentTicket
}

func NewResidentScheduler(limit int) *ResidentScheduler {
	if limit <= 0 {
		limit = DefaultResidentConcurrency
	}
	return &ResidentScheduler{limit: limit, running: make(map[string]bool)}
}

func (s *ResidentScheduler) Enqueue(childID, prompt string) ResidentTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ticket := ResidentTicket{Sequence: s.next, ChildID: childID, Prompt: prompt, ready: make(chan struct{})}
	s.pending = append(s.pending, ticket)
	return ticket
}

// Admit returns the oldest ticket whose child is not already active. An older
// ineligible ticket therefore cannot prevent a later independent child from
// using an available global slot.
func (s *ResidentScheduler) Admit() (ResidentTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= s.limit {
		return ResidentTicket{}, false
	}
	for i, ticket := range s.pending {
		if s.running[ticket.ChildID] {
			continue
		}
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		s.running[ticket.ChildID] = true
		s.active++
		close(ticket.ready)
		return ticket, true
	}
	return ResidentTicket{}, false
}

// Cancel removes an admitted-waiting ticket. Once admitted, the caller owns
// the active reservation and must Release it instead.
func (s *ResidentScheduler) Cancel(sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ticket := range s.pending {
		if ticket.Sequence != sequence {
			continue
		}
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		return true
	}
	return false
}

func (s *ResidentScheduler) Release(childID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running[childID] {
		return errors.New("resident scheduler: child has no active turn")
	}
	delete(s.running, childID)
	s.active--
	return nil
}
