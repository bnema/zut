package session

import "errors"

var (
	// ErrSeqTooOld reports event sequence too old.
	ErrSeqTooOld = errors.New("event sequence too old")
	// ErrClosed reports session closed.
	ErrClosed = errors.New("session closed")
	// ErrNotAttached reports session not attached.
	ErrNotAttached = errors.New("session not attached")
	// ErrBusy reports session busy.
	ErrBusy = errors.New("session busy")
	// ErrUnknownRequest reports unknown request.
	ErrUnknownRequest = errors.New("unknown request")
	// ErrLoopGuard reports inter-session message loop denied.
	ErrLoopGuard = errors.New("inter-session message loop denied")
	// ErrInvalidFork reports incompatible or unavailable fork inputs.
	ErrInvalidFork = errors.New("invalid fork request")
	// ErrForkParentNotFound reports a fork whose parent transcript is not
	// owned by a live session, so its policy cannot be inherited.
	ErrForkParentNotFound = errors.New("parent session not found")
	// ErrProfileEmpty reports an unassigned model profile slot.
	ErrProfileEmpty = errors.New("model profile empty")
)
