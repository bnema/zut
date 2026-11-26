package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/bnema/zut/packages/provider"
)

// ArchivedOptions configures ReadArchivedSessionText.
type ArchivedOptions struct {
	// Skip drops messages the caller never displays (hidden rows). nil keeps all.
	Skip func(provider.Message) bool
	// Limit keeps only the last Limit archived rows; <= 0 keeps all.
	Limit int
}

// ArchivedRow is one archived user or assistant text message.
type ArchivedRow struct {
	MsgSeq  int // same numbering as SessionTextRow.MsgSeq
	Message provider.Message
}

// ArchivedMark is a real compaction boundary placed after Rows[Index-1]
// (Index may be len(Rows)).
type ArchivedMark struct {
	Index        int
	TokensBefore int // from the summary Meta tokens_before; 0 if missing
}

// ArchivedHistory is the conversation that real compactions moved out of the
// model context.
type ArchivedHistory struct {
	Rows  []ArchivedRow
	Marks []ArchivedMark // ascending Index; the last one is the latest real compaction
	Older int            // archived rows dropped by Limit (they precede Rows)
}

// archiveCompaction describes one real compaction row (one with a summary).
type archiveCompaction struct {
	tokensBefore int
	// tail holds the fingerprints of the kept-tail text messages, in order.
	tail []uint64
}

// ReadArchivedSessionText returns the user/assistant text rows written before
// the latest real compaction, minus the kept tail that compaction carried
// into the live transcript. Compaction rows without a summary message (the
// first dynamic-context insert) are not real compactions and are ignored.
// Without a real compaction the history is empty. Oversized and malformed
// rows are skipped like ScanSessionText. The file is read twice so that, with
// Limit > 0, row memory stays bounded by Limit plus the kept tail.
func ReadArchivedSessionText(ctx context.Context, path string, opt ArchivedOptions) (ArchivedHistory, error) {
	keep := func(m provider.Message) bool { return opt.Skip == nil || !opt.Skip(m) }

	// Pass 1: find the latest real compaction and its kept tail.
	latestRow := -1
	var latest archiveCompaction
	err := scanArchiveLines(ctx, path, func(rowIdx int, line []byte, oversized bool) error {
		if oversized || !bytes.HasPrefix(line, []byte(`{"type":"compaction"`)) {
			return nil
		}
		if c, ok := decodeArchiveCompaction(line, keep); ok {
			latestRow, latest = rowIdx, c
		}
		return nil
	})
	if err != nil || latestRow < 0 {
		return ArchivedHistory{}, err
	}

	// Pass 2: collect archived rows before latestRow, bounded when limited.
	bound := 0
	if opt.Limit > 0 {
		bound = opt.Limit + len(latest.tail)
	}
	var rows []ArchivedRow
	var fps []uint64
	total := 0 // archived rows seen, including those trimmed by bound
	var marks []ArchivedMark
	msgSeq := 0
	err = scanArchiveLines(ctx, path, func(rowIdx int, line []byte, oversized bool) error {
		if rowIdx > latestRow {
			return errStopArchiveScan
		}
		if oversized {
			if bytes.HasPrefix(line, messageRowPrefix) {
				msgSeq++
			}
			return nil
		}
		var wire struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &wire) != nil {
			return nil
		}
		switch wire.Type {
		case "message":
			seq := msgSeq
			msgSeq++
			row, ok := decodeTextMessage(wire.Message)
			if !ok || !keep(row.Message) {
				return nil
			}
			rows = append(rows, ArchivedRow{MsgSeq: seq, Message: row.Message})
			fps = append(fps, row.Fingerprint)
			total++
			if bound > 0 && len(rows) > 2*bound {
				rows = append(rows[:0:0], rows[len(rows)-bound:]...)
				fps = append(fps[:0:0], fps[len(fps)-bound:]...)
			}
		case "compaction":
			if rowIdx == latestRow {
				return errStopArchiveScan
			}
			if c, ok := decodeArchiveCompaction(line, keep); ok {
				marks = append(marks, ArchivedMark{Index: total, TokensBefore: c.tokensBefore})
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopArchiveScan) {
		return ArchivedHistory{}, err
	}

	// Drop the matched kept-tail suffix.
	n := len(latest.tail)
	if n > len(fps) {
		n = len(fps)
	}
	for ; n > 0; n-- {
		if equalUint64s(fps[len(fps)-n:], latest.tail[:n]) {
			break
		}
	}
	rows = rows[:len(rows)-n]
	total -= n
	marks = append(marks, ArchivedMark{Index: total, TokensBefore: latest.tokensBefore})

	// Apply Limit and re-index marks.
	if opt.Limit > 0 && len(rows) > opt.Limit {
		rows = rows[len(rows)-opt.Limit:]
	}
	older := total - len(rows)
	out := ArchivedHistory{Rows: rows, Older: older}
	for _, m := range marks {
		if m.Index > total {
			m.Index = total
		}
		if m.Index < older {
			continue
		}
		m.Index -= older
		out.Marks = append(out.Marks, m)
	}
	return out, nil
}

var errStopArchiveScan = errors.New("stop archive scan")

func equalUint64s(a, b []uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

// decodeArchiveCompaction reports whether line is a real compaction row and
// returns its summary token count and kept-tail fingerprints.
func decodeArchiveCompaction(line []byte, keep func(provider.Message) bool) (archiveCompaction, bool) {
	var wire struct {
		Type     string            `json:"type"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(line, &wire) != nil || wire.Type != "compaction" {
		return archiveCompaction{}, false
	}
	var c archiveCompaction
	found := false
	for _, raw := range wire.Messages {
		if !found {
			msg, err := DecodeMessageJSON(raw)
			if err != nil || msg.Meta["compaction"] != "true" {
				continue
			}
			found = true
			c.tokensBefore, _ = strconv.Atoi(msg.Meta["tokens_before"])
			continue
		}
		if row, ok := decodeTextMessage(raw); ok && keep(row.Message) {
			c.tail = append(c.tail, row.Fingerprint)
		}
	}
	return c, found
}

// scanArchiveLines calls fn for each complete row after the meta row. rowIdx
// counts rows from 0 at the meta row. Oversized rows only carry a prefix.
func scanArchiveLines(ctx context.Context, path string, fn func(rowIdx int, line []byte, oversized bool) error) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read archived session text: open: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	for idx := 0; ; idx++ {
		if err := contextErr(ctx); err != nil {
			return err
		}
		line, n, complete, err := readScanLine(reader)
		if err != nil {
			return fmt.Errorf("read archived session text: %w", err)
		}
		if !complete {
			return nil
		}
		if idx == 0 {
			continue
		}
		if err := fn(idx, line, n > int64(len(line))); err != nil {
			return err
		}
	}
}
