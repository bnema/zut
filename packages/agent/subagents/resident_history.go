package subagents

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const (
	residentHistoryDefaultLimit = 200
	residentHistoryMaximumLimit = 1000
	residentHistoryPageBytes    = 1 << 20
	residentHistoryReadChunk    = 64 << 10
)

// ResidentHistoryItem is a provider-neutral finalized transcript item. The
// raw message and tool payloads are preserved so callers can render complete
// arguments/results without relying on a lossy text projection.
type ResidentHistoryItem struct {
	Type       string          `json:"type"`
	Time       time.Time       `json:"time"`
	Message    json.RawMessage `json:"message,omitempty"`
	ToolID     string          `json:"tool_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolArgs   json.RawMessage `json:"tool_args,omitempty"`
	ToolResult json.RawMessage `json:"tool_result,omitempty"`
}

// ResidentHistoryPage is a recent-first, bounded page of finalized history.
// OlderCursor is opaque and can be supplied to ReadResidentHistoryPage to load
// the adjacent older page. An empty cursor means this page reaches the oldest
// finalized content.
type ResidentHistoryPage struct {
	Items       []ResidentHistoryItem
	OlderCursor string
}

type residentHistoryCursor struct {
	Generation string `json:"generation"`
	Offset     int64  `json:"offset"`
	Size       int64  `json:"size"`
}

type residentHistoryEntry struct {
	item   ResidentHistoryItem
	offset int64
}

// ReadResidentHistoryPage reads a bounded suffix of one resident journal. It
// never follows arbitrary paths at public manager boundaries; callers outside
// this package should use ResidentManager.HistoryPage with a child ID.
//
// A page starts at a user or assistant record. That preserves the adjacent
// assistant/tool-call/tool-result group when paging backward. Incomplete final
// lines are deliberately deferred until the writer completes them.
func ReadResidentHistoryPage(dir, olderCursor string, limit int) (ResidentHistoryPage, error) {
	if limit <= 0 {
		limit = residentHistoryDefaultLimit
	}
	if limit > residentHistoryMaximumLimit {
		limit = residentHistoryMaximumLimit
	}
	f, err := os.Open(filepath.Join(dir, residentTranscriptName))
	if err != nil {
		return ResidentHistoryPage{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ResidentHistoryPage{}, err
	}
	generation, err := residentHistoryGeneration(f, info, info.Size())
	if err != nil {
		return ResidentHistoryPage{}, err
	}
	end := info.Size()
	if olderCursor != "" {
		cursor, err := decodeResidentHistoryCursor(olderCursor)
		if err != nil {
			return ResidentHistoryPage{}, err
		}
		cursorGeneration, generationErr := residentHistoryGeneration(f, info, cursor.Size)
		if generationErr != nil {
			return ResidentHistoryPage{}, generationErr
		}
		if cursor.Generation != cursorGeneration || cursor.Size > end || cursor.Offset <= 0 || cursor.Offset > end {
			return ResidentHistoryPage{}, errors.New("resident history: cursor is no longer valid")
		}
		end = cursor.Offset
	}
	return readResidentHistorySuffix(f, end, limit, generation, info.Size())
}

func residentHistoryGeneration(f *os.File, info os.FileInfo, originalSize int64) (string, error) {
	prefixSize := originalSize
	if prefixSize > 4096 {
		prefixSize = 4096
	}
	prefix := make([]byte, prefixSize)
	if prefixSize > 0 {
		read, err := f.ReadAt(prefix, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("resident history: read journal generation: %w", err)
		}
		prefix = prefix[:read]
	}
	sum := sha256.Sum256(prefix)
	return fmt.Sprintf("%s:%x", residentHistoryFileIdentity(info), sum[:8]), nil
}

// residentHistoryFileIdentity uses only stable file identity fields when the
// host exposes them. Dev/Ino cover Unix; creation time is the portable
// fallback for Windows metadata. The prefix digest remains the corruption
// check when a platform offers neither.
func residentHistoryFileIdentity(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	var parts []string
	for _, field := range []string{"Dev", "Ino", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow", "CreationTime"} {
		v := value.FieldByName(field)
		if !v.IsValid() || !v.CanInterface() {
			continue
		}
		parts = append(parts, field+"="+fmt.Sprint(v.Interface()))
	}
	return strings.Join(parts, ",")
}

func decodeResidentHistoryCursor(encoded string) (residentHistoryCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return residentHistoryCursor{}, errors.New("resident history: invalid cursor")
	}
	var cursor residentHistoryCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Generation == "" || cursor.Size <= 0 {
		return residentHistoryCursor{}, errors.New("resident history: invalid cursor")
	}
	return cursor, nil
}

func encodeResidentHistoryCursor(generation string, offset, size int64) string {
	data, _ := json.Marshal(residentHistoryCursor{Generation: generation, Offset: offset, Size: size})
	return base64.RawURLEncoding.EncodeToString(data)
}

func readResidentHistorySuffix(f *os.File, end int64, limit int, generation string, size int64) (ResidentHistoryPage, error) {
	if end == 0 {
		return ResidentHistoryPage{}, nil
	}
	start := end
	data := make([]byte, 0, residentHistoryReadChunk)
	for {
		readSize := int64(residentHistoryReadChunk)
		if readSize > start {
			readSize = start
		}
		if int64(len(data))+readSize > residentHistoryPageBytes {
			return ResidentHistoryPage{}, errors.New("resident history: page byte budget exceeded")
		}
		start -= readSize
		chunk := make([]byte, readSize)
		read, err := f.ReadAt(chunk, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return ResidentHistoryPage{}, fmt.Errorf("resident history: read journal suffix: %w", err)
		}
		chunk = chunk[:read]
		data = append(chunk, data...)

		entries, err := decodeResidentHistoryEntries(data, start, start == 0)
		if err != nil {
			return ResidentHistoryPage{}, err
		}
		first := residentHistoryPageStart(entries, limit)
		if first >= 0 {
			page := ResidentHistoryPage{Items: make([]ResidentHistoryItem, len(entries)-first)}
			for index := first; index < len(entries); index++ {
				page.Items[index-first] = cloneResidentHistoryItem(entries[index].item)
			}
			if start > 0 || first > 0 {
				page.OlderCursor = encodeResidentHistoryCursor(generation, entries[first].offset, size)
			}
			return page, nil
		}
		if start == 0 {
			if len(entries) == 0 {
				return ResidentHistoryPage{}, nil
			}
			return ResidentHistoryPage{}, errors.New("resident history: tool group has no message boundary")
		}
	}
}

func decodeResidentHistoryEntries(data []byte, start int64, startsAtBeginning bool) ([]residentHistoryEntry, error) {
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		return nil, nil
	}
	complete := data[:lastNewline+1]
	base := start
	if !startsAtBeginning {
		firstNewline := bytes.IndexByte(complete, '\n')
		if firstNewline < 0 {
			return nil, nil
		}
		base += int64(firstNewline + 1)
		complete = complete[firstNewline+1:]
	}
	entries := make([]residentHistoryEntry, 0)
	for len(complete) > 0 {
		newline := bytes.IndexByte(complete, '\n')
		if newline < 0 {
			break
		}
		line := complete[:newline]
		lineOffset := base
		base += int64(newline + 1)
		complete = complete[newline+1:]
		if len(line) > residentMaxRecordBytes {
			return nil, errors.New("resident history: record too large")
		}
		var record residentRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("resident history: decode journal record: %w", err)
		}
		item, include, err := residentHistoryItem(record)
		if err != nil {
			return nil, err
		}
		if include {
			entries = append(entries, residentHistoryEntry{item: item, offset: lineOffset})
		}
	}
	return entries, nil
}

func residentHistoryItem(record residentRecord) (ResidentHistoryItem, bool, error) {
	item := ResidentHistoryItem{Type: record.Type, Time: record.Time}
	switch record.Type {
	case residentRecordUser, residentRecordAssistant:
		if len(record.Message) == 0 || !json.Valid(record.Message) {
			return ResidentHistoryItem{}, false, errors.New("resident history: malformed message record")
		}
		item.Message = append(json.RawMessage(nil), record.Message...)
	case residentRecordToolCall:
		if record.ToolID == "" || record.ToolName == "" || !json.Valid(record.ToolArgs) {
			return ResidentHistoryItem{}, false, errors.New("resident history: malformed tool call record")
		}
		item.ToolID, item.ToolName = record.ToolID, record.ToolName
		item.ToolArgs = append(json.RawMessage(nil), record.ToolArgs...)
	case residentRecordToolResult:
		if record.ToolID == "" || !json.Valid(record.ToolResult) {
			return ResidentHistoryItem{}, false, errors.New("resident history: malformed tool result record")
		}
		item.ToolID, item.ToolResult = record.ToolID, append(json.RawMessage(nil), record.ToolResult...)
	default:
		return ResidentHistoryItem{}, false, nil
	}
	return item, true, nil
}

func residentHistoryPageStart(entries []residentHistoryEntry, limit int) int {
	first, fallback := -1, -1
	for index := range entries {
		if entries[index].item.Type != residentRecordUser && entries[index].item.Type != residentRecordAssistant {
			continue
		}
		fallback = index
		if len(entries)-index <= limit {
			first = index
		}
	}
	if first < 0 {
		return fallback
	}
	return first
}

func cloneResidentHistoryItem(item ResidentHistoryItem) ResidentHistoryItem {
	item.Message = append(json.RawMessage(nil), item.Message...)
	item.ToolArgs = append(json.RawMessage(nil), item.ToolArgs...)
	item.ToolResult = append(json.RawMessage(nil), item.ToolResult...)
	return item
}

// ResidentHistoryMessages converts finalized journal items into the same
// provider-neutral message structures used by tui.View. Tool-call records
// duplicate calls already embedded in an assistant message, so they are only
// synthesized when the assistant record is absent (for recovery resilience).
func ResidentHistoryMessages(items []ResidentHistoryItem) ([]provider.Message, error) {
	messages := make([]provider.Message, 0, len(items))
	calls := make(map[string]struct{})
	for _, item := range items {
		switch item.Type {
		case residentRecordUser, residentRecordAssistant:
			message, err := core.DecodeMessageJSON(item.Message)
			if err != nil {
				return nil, fmt.Errorf("resident history: decode message: %w", err)
			}
			for _, content := range message.Content {
				if call, ok := content.(provider.ToolCallBlock); ok {
					calls[call.ID] = struct{}{}
				}
			}
			messages = append(messages, message)
		case residentRecordToolCall:
			if _, exists := calls[item.ToolID]; exists {
				continue
			}
			messages = append(messages, provider.Message{Role: provider.RoleAssistant, Time: item.Time, Content: []provider.Content{provider.ToolCallBlock{ID: item.ToolID, Name: item.ToolName, Arguments: append(json.RawMessage(nil), item.ToolArgs...)}}})
			calls[item.ToolID] = struct{}{}
		case residentRecordToolResult:
			var result struct {
				Content []json.RawMessage    `json:"Content"`
				IsError bool                 `json:"IsError"`
				Timing  *provider.ToolTiming `json:"Timing"`
			}
			if err := json.Unmarshal(item.ToolResult, &result); err != nil || len(result.Content) == 0 {
				if err == nil {
					err = errors.New("missing content")
				}
				return nil, fmt.Errorf("resident history: decode tool result: %w", err)
			}
			raw, err := json.Marshal(struct {
				Role    provider.Role `json:"role"`
				Time    time.Time     `json:"time"`
				Content []struct {
					CallID  string               `json:"call_id"`
					Content []json.RawMessage    `json:"content"`
					IsError bool                 `json:"is_error"`
					Timing  *provider.ToolTiming `json:"timing,omitempty"`
				} `json:"content"`
			}{Role: provider.RoleTool, Time: item.Time, Content: []struct {
				CallID  string               `json:"call_id"`
				Content []json.RawMessage    `json:"content"`
				IsError bool                 `json:"is_error"`
				Timing  *provider.ToolTiming `json:"timing,omitempty"`
			}{{CallID: item.ToolID, Content: result.Content, IsError: result.IsError, Timing: result.Timing}}})
			if err != nil {
				return nil, err
			}
			message, err := core.DecodeMessageJSON(raw)
			if err != nil {
				return nil, fmt.Errorf("resident history: decode tool result message: %w", err)
			}
			messages = append(messages, message)
		}
	}
	return messages, nil
}

// ReadResidentTranscriptMessages rebuilds finalized conversation state for an
// explicit resident resume. It reads the authoritative transcript, unlike the
// bounded UI pager, and must therefore only be used at the resume boundary.
func ReadResidentTranscriptMessages(dir string) ([]provider.Message, error) {
	records, err := ReadResidentJournal(filepath.Join(dir, residentTranscriptName))
	if err != nil {
		return nil, err
	}
	items := make([]ResidentHistoryItem, 0, len(records))
	for _, record := range records {
		item, include, err := residentHistoryItem(record)
		if err != nil {
			return nil, err
		}
		if include {
			items = append(items, item)
		}
	}
	return ResidentHistoryMessages(items)
}

// ReadResidentHistory returns the recent finalized journal items, bounded by
// item count. Public callers provide a child directory obtained from manager
// state, never an arbitrary user-controlled path.
func ReadResidentHistory(dir string, limit int) ([]ResidentHistoryItem, error) {
	page, err := ReadResidentHistoryPage(dir, "", limit)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// ResidentHistoryDir resolves a child ID beneath root without accepting path
// traversal. It is kept separate from the reader so manager APIs can apply
// their own visibility checks before exposing history.
func ResidentHistoryDir(root, childID string) (string, error) {
	childID = strings.TrimSpace(childID)
	if childID == "" || filepath.Base(childID) != childID {
		return "", errors.New("resident history: invalid child ID")
	}
	dir := filepath.Join(root, childID)
	if info, err := os.Stat(dir); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", errors.New("resident history: child path is not a directory")
	}
	return dir, nil
}
