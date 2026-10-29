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
)
