package modes

import (
	"fmt"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
)

// residentSubagentsDialog is intentionally only a discovery/selection list.
// Detailed transcript, paging, and composition belong to residentChildSession.
type residentSubagentsDialog struct {
	active   bool
	manager  *subagents.ResidentManager
	rows     []subagents.ResidentSnapshot
	selected int // absolute index in the sorted manager projection
	offset   int
	total    int
}

func newResidentSubagentsDialog() *residentSubagentsDialog { return &residentSubagentsDialog{} }

func (d *residentSubagentsDialog) Open(manager *subagents.ResidentManager) {
	d.active, d.manager, d.selected, d.offset, d.total = true, manager, 0, 0, 0
	d.refresh(12)
}

func (d *residentSubagentsDialog) Active() bool { return d != nil && d.active }
func (d *residentSubagentsDialog) Close() {
	if d != nil {
		d.active = false
	}
}

func (d *residentSubagentsDialog) refresh(limit int) {
	if d == nil || d.manager == nil {
		d.rows = nil
		d.total = 0
		return
	}
	if limit < 1 {
		limit = 1
	}
	_, total := d.manager.SnapshotPage(0, 0)
	d.total = total
	if d.selected >= d.total {
		d.selected = d.total - 1
	}
	if d.selected < 0 || d.total == 0 {
		d.selected = 0
	}
	if d.selected < d.offset {
		d.offset = d.selected
	}
	if d.selected >= d.offset+limit {
		d.offset = d.selected - limit + 1
	}
	if d.offset < 0 {
		d.offset = 0
	}
	d.rows, d.total = d.manager.SnapshotPage(d.offset, limit)
}

func (d *residentSubagentsDialog) HandleKey(k tui.Key) (openID string) {
	if d == nil {
		return ""
	}
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
	maxRows := height - 3 // title, hint, and page indicator/empty state
	if maxRows < 1 {
		maxRows = 1
	}
	d.refresh(maxRows)
	lines := []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn"}
	if len(d.rows) == 0 {
		return append(lines, "", "  No resident subagents.")
	}
	for index, row := range d.rows {
		marker := "  "
		if d.offset+index == d.selected {
			marker = theme.AccentBar(theme.Accent) + " "
		}
		label := fmt.Sprintf("%s%s  %s  %s/%s", marker, row.ID, row.State, row.Provider, row.Model)
		if row.Profile != "" {
			label += "  " + row.Profile
		}
		if row.WorkspaceMode != "" {
			label += "  " + string(row.WorkspaceMode)
		}
		if row.Required {
			label += "  required"
		}
		lines = append(lines, label)
	}
	if d.total > len(d.rows) {
		lines = append(lines, fmt.Sprintf("  %d-%d of %d", d.offset+1, d.offset+len(d.rows), d.total))
	}
	return lines
}
