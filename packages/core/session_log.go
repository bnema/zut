package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bnema/zut/packages/provider"
)

// sessionLogRow is the validated, hydrated representation shared by complete
// session readers. It keeps the JSONL format private while giving each reader
// one typed stream to reduce into its own projection.
type sessionLogRow struct {
	typeName   string
	meta       *SessionMeta
	message    *provider.Message
	messages   []provider.Message
	cumulative *provider.Usage
	title      *string
	extension  string
	state      json.RawMessage
}

// forEachSessionLogRowContext owns the strict persisted-row contract: JSONL
// framing, the leading meta row, known row types, required fields, and message
// content hydration. Callers only implement projection-specific reduction.
func forEachSessionLogRowContext(ctx context.Context, r io.Reader, fn func(sessionLogRow) error) error {
	sawMeta := false
	err := forEachStrictJSONLLineContext(ctx, r, func(line []byte, lineNo int) error {
		row, err := decodeSessionLogRow(line)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if !sawMeta && row.typeName != "meta" {
			return fmt.Errorf("line %d: first row is not meta", lineNo)
		}
		if row.typeName == "meta" {
			sawMeta = true
		}
		return fn(row)
	})
	if err != nil {
		return err
	}
	if !sawMeta {
		return fmt.Errorf("file is empty")
	}
	return nil
}

func decodeSessionLogRow(line []byte) (sessionLogRow, error) {
	var wire struct {
		Type       string          `json:"type"`
		Meta       *SessionMeta    `json:"meta"`
		Message    json.RawMessage `json:"message"`
		Messages   json.RawMessage `json:"messages"`
		Cumulative *provider.Usage `json:"cumulative"`
		Title      *string         `json:"title"`
		Extension  string          `json:"extension"`
		State      json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(line, &wire); err != nil {
		return sessionLogRow{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if wire.Type == "" {
		return sessionLogRow{}, fmt.Errorf("missing row type")
	}

	row := sessionLogRow{
		typeName:   wire.Type,
		cumulative: wire.Cumulative,
		title:      wire.Title,
		extension:  wire.Extension,
		state:      wire.State,
	}
	switch wire.Type {
	case "meta":
		if wire.Meta == nil || wire.Meta.ID == "" {
			return sessionLogRow{}, fmt.Errorf("meta row has no session id")
		}
		row.meta = wire.Meta
	case "message":
		if len(wire.Message) == 0 || bytes.Equal(bytes.TrimSpace(wire.Message), []byte("null")) {
			return sessionLogRow{}, fmt.Errorf("invalid message row: message row has no message")
		}
		message, err := hydrateMessageObject(wire.Message)
		if err != nil {
			return sessionLogRow{}, fmt.Errorf("invalid message row: %w", err)
		}
		row.message = &message
	case "compaction":
		messages, err := hydrateCompactionMessages(wire.Messages)
		if err != nil {
			return sessionLogRow{}, fmt.Errorf("invalid compaction row: %w", err)
		}
		row.messages = messages
	case "usage":
		if wire.Cumulative == nil {
			return sessionLogRow{}, fmt.Errorf("usage row has no cumulative usage")
		}
	case "rename":
		if wire.Title == nil {
			return sessionLogRow{}, fmt.Errorf("rename row has no title")
		}
	case "extension_state":
		// Invalid or oversized extension snapshots are ignored by reducers for
		// compatibility with existing sessions; transcript rows stay strict.
	default:
		return sessionLogRow{}, fmt.Errorf("unknown row type %q", wire.Type)
	}
	return row, nil
}

func hydrateCompactionMessages(raw json.RawMessage) ([]provider.Message, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// Older writers omitted the field for an empty compaction and some
		// wrote it as null. Both represent a valid empty replacement.
		return []provider.Message{}, nil
	}
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(raw, &rawMessages); err != nil {
		return nil, fmt.Errorf("invalid messages: %w", err)
	}
	messages := make([]provider.Message, 0, len(rawMessages))
	for idx, rawMessage := range rawMessages {
		message, err := hydrateMessageObject(rawMessage)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", idx, err)
		}
		messages = append(messages, message)
	}
	return messages, nil
}
