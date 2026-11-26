package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// maxSessionSearchRowBytes bounds one JSONL row read for search text.
const maxSessionSearchRowBytes = 1 << 20

// oversizedRowPrefixBytes is how much of an oversized row is kept to classify
// it. Writers marshal sessionLine, whose "type" field always comes first.
const oversizedRowPrefixBytes = 64

var messageRowPrefix = []byte(`{"type":"message"`)

// SessionTextRow is the searchable text of one persisted user or assistant
// message row.
type SessionTextRow struct {
	// MsgSeq is the ordinal of the raw "message" row in the file, counting
	// every role and oversized message rows. Compaction rows are not counted.
	MsgSeq int
	Role   provider.Role
	Time   time.Time
	// Texts holds the non-empty provider text blocks in order.
	Texts []string
	// Offset and Len are the byte range of the JSONL row, excluding '\n'.
	Offset, Len int64
	// Fingerprint is MessageTextFingerprint of the decoded message.
	Fingerprint uint64
	// Message is the decoded message, for caller-side filtering.
	Message provider.Message
}

// SessionScanCursor records where an incremental ScanSessionText resumes.
type SessionScanCursor struct {
	// Offset points right after the last consumed newline.
	Offset int64
	// MsgSeq is the next message ordinal.
	MsgSeq int
	// MetaID and HeadHash identify the file (FNV-64a of the first row bytes).
	MetaID   string
	HeadHash uint64
	// Reset is set on a returned cursor when the input cursor no longer
	// matched the file (replaced or truncated) and the scan restarted at 0.
	// Callers must then discard rows previously indexed for this path.
	// It is ignored on input. Reset is only known on return, after fn has
	// already received the rescanned rows, and it is also set on error
	// returns (callback error, cancellation) of a restarted scan.
	Reset bool
	// SawMetaChange is set on a returned cursor when the consumed rows
	// include anything that can change session metadata, title, or
	// listability: meta, rename, or compaction rows, and rows that are
	// malformed, untyped, or oversized and not message rows. Callers caching
	// a summary must refresh it then. It is ignored on input.
	SawMetaChange bool
}

// MessageTextFingerprint hashes the role and the non-empty text blocks of m.
// Indexers and UI resolvers must both use it to match persisted rows.
func MessageTextFingerprint(m provider.Message) uint64 {
	h := fnv.New64a()
	h.Write([]byte(m.Role))
	for _, text := range messageTexts(m) {
		h.Write([]byte{0})
		h.Write([]byte(text))
	}
	return h.Sum64()
}

func messageTexts(m provider.Message) []string {
	var texts []string
	for _, block := range m.Content {
		if tb, ok := block.(provider.TextBlock); ok && tb.Text != "" {
			texts = append(texts, tb.Text)
		}
	}
	return texts
}

// ScanSessionText streams user/assistant text rows starting at from. It stops
// at the last complete line, so a partially written trailing row is left for
// a later scan. Rows larger than maxSessionSearchRowBytes are skipped (still
// counted in MsgSeq when they start with a message type prefix). Malformed
// rows are skipped too. Oversized rows whose bytes do not start with
// {"type":"message" (all zut writers marshal sessionLine, type first) are not
// counted. The returned SessionMeta is always the first-row meta, so full and
// resumed scans agree; later meta rows are ignored.
func ScanSessionText(ctx context.Context, path string, from SessionScanCursor, fn func(SessionTextRow) error) (SessionScanCursor, SessionMeta, error) {
	from.Reset = false
	from.SawMetaChange = false
	if err := contextErr(ctx); err != nil {
		return from, SessionMeta{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return from, SessionMeta{}, fmt.Errorf("scan session text: open: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return from, SessionMeta{}, fmt.Errorf("scan session text: stat: %w", err)
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	head, headLen, complete, err := readScanLine(reader)
	if err != nil {
		return from, SessionMeta{}, fmt.Errorf("scan session text: %w", err)
	}
	cur := SessionScanCursor{}
	var meta SessionMeta
	if !complete {
		// No complete first row yet: nothing is consumable.
		cur.Reset = from.Offset > 0
		return cur, meta, nil
	}
	cur.HeadHash = fnvBytes(head)
	if m, ok := decodeMetaRow(head); ok {
		meta = m
		cur.MetaID = m.ID
	}

	start := int64(0)
	if from.Offset > 0 {
		if from.Offset <= info.Size() && from.MetaID == cur.MetaID && from.HeadHash == cur.HeadHash {
			start = from.Offset
			cur.MsgSeq = from.MsgSeq
		} else {
			cur.Reset = true
		}
	}
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return from, SessionMeta{}, fmt.Errorf("scan session text: seek: %w", err)
		}
		reader.Reset(file)
		cur.Offset = start
	} else {
		cur.Offset = headLen + 1
	}

	for {
		if err := contextErr(ctx); err != nil {
			return cur, meta, err
		}
		line, rowLen, complete, err := readScanLine(reader)
		if err != nil {
			return cur, meta, fmt.Errorf("scan session text: %w", err)
		}
		if !complete {
			return cur, meta, nil
		}
		rowOff := cur.Offset
		if rowLen > int64(len(line)) {
			if bytes.HasPrefix(line, messageRowPrefix) {
				cur.MsgSeq++
			} else {
				cur.SawMetaChange = true
			}
			cur.Offset += rowLen + 1
			continue
		}
		cur.Offset += rowLen + 1
		var wire textRowWire
		if json.Unmarshal(line, &wire) != nil {
			cur.SawMetaChange = true
			continue
		}
		switch wire.Type {
		case "meta", "rename", "compaction", "":
			cur.SawMetaChange = true
		case "message":
			seq := cur.MsgSeq
			cur.MsgSeq++
			row, ok := decodeTextMessage(wire.Message)
			if !ok {
				continue
			}
			row.MsgSeq, row.Offset, row.Len = seq, rowOff, rowLen
			if err := fn(row); err != nil {
				// The failed row is not consumed so a retry re-emits it.
				cur.Offset, cur.MsgSeq = rowOff, seq
				return cur, meta, err
			}
		}
	}
}

// ReadSessionTextRowAt reads and decodes the row at [off, off+n) of an open
// session file. MsgSeq is 0. Callers reading several rows open the file once.
func ReadSessionTextRowAt(r io.ReaderAt, off, n int64) (SessionTextRow, error) {
	if off < 0 || n <= 0 || n > maxSessionSearchRowBytes {
		return SessionTextRow{}, fmt.Errorf("read session text row: invalid range %d+%d", off, n)
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, off); err != nil {
		return SessionTextRow{}, fmt.Errorf("read session text row: %w", err)
	}
	var wire textRowWire
	if err := json.Unmarshal(buf, &wire); err != nil || wire.Type != "message" {
		return SessionTextRow{}, errors.New("read session text row: not a message row")
	}
	row, ok := decodeTextMessage(wire.Message)
	if !ok {
		return SessionTextRow{}, errors.New("read session text row: no user or assistant text")
	}
	row.Offset, row.Len = off, n
	return row, nil
}

// SessionHeadHash returns the HeadHash that ScanSessionText records for r:
// FNV-64a of the first complete row. ok is false without a complete row.
func SessionHeadHash(r io.Reader) (hash uint64, ok bool, err error) {
	head, _, complete, err := readScanLine(bufio.NewReaderSize(r, 64*1024))
	if err != nil || !complete {
		return 0, false, err
	}
	return fnvBytes(head), true, nil
}

// textRowWire is the one decode of a JSONL row done by the text scanner.
type textRowWire struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

func decodeTextMessage(raw json.RawMessage) (SessionTextRow, bool) {
	// Cheap role peek first: tool rows (often large) skip the full decode.
	// DecodeMessageJSON keeps the role verbatim, so this filter is the same
	// one applied after decoding.
	if len(raw) == 0 {
		return SessionTextRow{}, false
	}
	var peek struct {
		Role provider.Role `json:"role"`
	}
	if json.Unmarshal(raw, &peek) != nil || (peek.Role != provider.RoleUser && peek.Role != provider.RoleAssistant) {
		return SessionTextRow{}, false
	}
	msg, err := DecodeMessageJSON(raw)
	if err != nil || (msg.Role != provider.RoleUser && msg.Role != provider.RoleAssistant) {
		return SessionTextRow{}, false
	}
	texts := messageTexts(msg)
	if len(texts) == 0 {
		return SessionTextRow{}, false
	}
	return SessionTextRow{Role: msg.Role, Time: msg.Time, Texts: texts, Fingerprint: MessageTextFingerprint(msg), Message: msg}, true
}

func decodeMetaRow(line []byte) (SessionMeta, bool) {
	var row struct {
		Type string       `json:"type"`
		Meta *SessionMeta `json:"meta"`
	}
	if json.Unmarshal(line, &row) != nil || row.Type != "meta" || row.Meta == nil {
		return SessionMeta{}, false
	}
	return *row.Meta, true
}

func fnvBytes(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// readScanLine reads one '\n'-terminated row. n is the raw row length
// without the newline. complete is false when EOF arrives before a newline.
// For rows above maxSessionSearchRowBytes only the first
// oversizedRowPrefixBytes bytes are returned, so int64(len(line)) < n.
func readScanLine(reader *bufio.Reader) (line []byte, n int64, complete bool, err error) {
	oversized := false
	for {
		fragment, readErr := reader.ReadSlice('\n')
		n += int64(len(fragment))
		if !oversized {
			line = append(line, fragment...)
			// The row content (without its '\n') may be up to the max.
			content := len(line)
			if readErr == nil {
				content--
			}
			if content > maxSessionSearchRowBytes {
				oversized = true
				line = append([]byte(nil), line[:oversizedRowPrefixBytes]...)
			}
		}
		switch {
		case readErr == nil:
			n--
			if !oversized {
				line = line[:len(line)-1]
			}
			return line, n, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return nil, 0, false, nil
		default:
			return nil, 0, false, readErr
		}
	}
}
