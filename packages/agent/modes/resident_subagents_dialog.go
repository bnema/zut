package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
)

// residentSubagentsDialog is intentionally only a discovery/selection list.
// Detailed transcript, paging, and composition belong to residentChildSession.
// Rows come from the session port; the daemon owns the resident manager.
//
// Every method runs under the owner's lock (Interactive.mu). Port reads never
// run there: Open, paging, and Invalidate start one asynchronous fetch whose
// result is stored under that lock and followed by notify. Render only reads
// the cached window.
type residentSubagentsDialog struct {
	lock   sync.Locker
	notify func()

	active   bool
	port     session.Port
	rows     []subagents.ResidentSnapshot
	selected int // absolute index in the sorted session projection
	offset   int
	total    int
	err      error
	loaded   bool
	// limit is the window size requested by the last Render.
	limit int
	// gen identifies the current fetch; stale results are discarded.
	gen     uint64
	pending bool
	// fetched is the window the cache holds; stale forces a refetch.
	fetchedOffset, fetchedLimit int
	stale                       bool
	cancel                      context.CancelFunc
}

const (
	residentDialogTimeout      = 2 * time.Second
	residentDialogInitialLimit = 12
)

func newResidentSubagentsDialog(lock sync.Locker, notify func()) *residentSubagentsDialog {
	return &residentSubagentsDialog{lock: lock, notify: notify}
}

func (d *residentSubagentsDialog) Open(port session.Port) {
	d.stop()
	d.active, d.port, d.selected, d.offset, d.total, d.err = true, port, 0, 0, 0, nil
	d.rows, d.loaded, d.limit = nil, false, residentDialogInitialLimit
	d.schedule()
}

func (d *residentSubagentsDialog) Active() bool { return d != nil && d.active }
func (d *residentSubagentsDialog) Close() {
	if d != nil {
		d.active = false
		d.stop()
	}
}

// Invalidate marks the cached window stale (a resident info event arrived)
// and refetches it if the dialog is open.
func (d *residentSubagentsDialog) Invalidate() {
	if d == nil || !d.active {
		return
	}
	d.stale = true
	d.schedule()
}

func (d *residentSubagentsDialog) stop() {
	d.gen++
	d.pending = false
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
}

// schedule starts a fetch of the current window unless the cache already
// holds it or the same window is in flight.
func (d *residentSubagentsDialog) schedule() {
	if d == nil || !d.active || d.port == nil {
		return
	}
	limit := max(d.limit, 0)
	d.clampSelection(limit)
	if !d.stale && d.loaded && d.fetchedOffset == d.offset && d.fetchedLimit == limit {
		return
	}
	if d.pending && d.fetchedOffset == d.offset && d.fetchedLimit == limit && !d.stale {
		return
	}
	d.stop()
	d.pending, d.stale = true, false
	d.fetchedOffset, d.fetchedLimit = d.offset, limit
	gen, port, offset := d.gen, d.port, d.offset
	ctx, cancel := context.WithTimeout(context.Background(), residentDialogTimeout)
	d.cancel = cancel
	go func() {
		defer cancel()
		rows, total, err := port.ResidentList(ctx, offset, limit)
		d.lock.Lock()
		if d.gen != gen {
			d.lock.Unlock()
			return
		}
		d.pending, d.cancel, d.loaded = false, nil, true
		if err != nil {
			d.rows, d.err = nil, err
		} else {
			d.rows, d.total, d.err = rows, total, nil
			if limit == 0 {
				d.rows = nil
			}
			// The count may have shrunk; re-anchor and refetch if needed.
			d.schedule()
		}
		d.lock.Unlock()
		if d.notify != nil {
			d.notify()
		}
	}()
}

// clampSelection keeps the selection inside the last known total and the
// visible window inside the selection.
func (d *residentSubagentsDialog) clampSelection(limit int) {
	if d.selected >= d.total {
		d.selected = d.total - 1
	}
	if d.selected < 0 || d.total == 0 {
		d.selected = 0
	}
	if limit > 0 {
		if d.selected < d.offset {
			d.offset = d.selected
		}
		if d.selected >= d.offset+limit {
			d.offset = d.selected - limit + 1
		}
	}
	if d.offset < 0 {
		d.offset = 0
	}
}

func (d *residentSubagentsDialog) HandleKey(k tui.Key) (openID string) {
	if d == nil {
		return ""
	}
	defer d.schedule()
	switch k.Kind {
	case tui.KeyUp:
		if d.selected > 0 {
			d.selected--
		}
	case tui.KeyDown:
		if d.selected+1 < d.total {
			d.selected++
		}
	case tui.KeyEnter:
		if len(d.rows) > 0 && d.selected >= d.offset && d.selected < d.offset+len(d.rows) {
			return d.rows[d.selected-d.offset].ID
		}
	}
	return ""
}

func (d *residentSubagentsDialog) Render(theme tui.Theme, width, height int) []string {
	if d == nil {
		return nil
	}
	// Reserve two lines per child so usage metadata never pushes the dialog
	// beyond its content height. Children without usage simply consume less.
	maxRows := (height - 3) / 2 // title, hint, and page indicator/empty state
	if maxRows < 0 {
		maxRows = 0
	}
	if maxRows != d.limit {
		d.limit = maxRows
		d.schedule()
	}
	lines := []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn"}
	if d.err != nil {
		lines = append(lines, "  "+truncateResidentSubagentIndicator("unavailable: "+sanitizeSessionTreeText(d.err.Error()), width-2))
		return limitResidentSubagentsDialogHeight(lines, height)
	}
	if !d.loaded && d.port != nil {
		return limitResidentSubagentsDialogHeight(append(lines, "  Loading…"), height)
	}
	if len(d.rows) == 0 {
		if d.total > 0 {
			return limitResidentSubagentsDialogHeight(lines, height)
		}
		if height == len(lines)+1 {
			lines = append(lines, "  No resident subagents.")
		} else {
			lines = append(lines, "", "  No resident subagents.")
		}
		return limitResidentSubagentsDialogHeight(lines, height)
	}
	for index, row := range d.rows {
		marker := "  "
		if d.offset+index == d.selected {
			marker = theme.AccentBar(theme.Accent) + " "
		}
		name := residentDisplayName(row)
		label := fmt.Sprintf("%s  %s  %s  %s/%s  %s", name, residentDisplayState(row.State), formatResidentUpdatedAt(row.UpdatedAt, time.Now()), row.Provider, row.Model, shortResidentID(row.ID))
		if row.WorkspaceMode != "" {
			label += "  " + string(row.WorkspaceMode)
		}
		if row.Required {
			label += "  required"
		}
		lines = append(lines, marker+truncateResidentSubagentIndicator(label, width-2))
		if metadata := residentUsageMetadata(row); metadata != "" {
			metadata = truncateResidentSubagentIndicator(metadata, width-4)
			if metadata != "" {
				lines = append(lines, "    "+metadata)
			}
		}
	}
	if d.total > len(d.rows) {
		lines = append(lines, fmt.Sprintf("  %d-%d of %d", d.offset+1, d.offset+len(d.rows), d.total))
	}
	return limitResidentSubagentsDialogHeight(lines, height)
}

func limitResidentSubagentsDialogHeight(lines []string, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) <= height {
		return lines
	}
	return lines[:height]
}

func residentDisplayName(row subagents.ResidentSnapshot) string {
	if name := sanitizeSessionTreeText(row.Profile); name != "" {
		return name
	}
	if model := sanitizeSessionTreeText(row.Model); model != "" {
		return model
	}
	return "subagent"
}

func residentDisplayState(state subagents.ResidentState) string {
	if state == subagents.ResidentIdle {
		return "completed"
	}
	return string(state)
}

func shortResidentID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func formatResidentUpdatedAt(updatedAt, now time.Time) string {
	if updatedAt.IsZero() {
		return "time unknown"
	}
	if now.IsZero() {
		now = time.Now()
	}
	elapsed := now.Sub(updatedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}
