package daemonproto

import "github.com/bnema/zut/packages/agent/subagents"

// ResidentListParams are optional paging parameters for resident_list.
type ResidentListParams struct {
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

// ResidentListData carries recent-first snapshots and the unpaged count.
type ResidentListData struct {
	Residents []subagents.ResidentSnapshot `json:"residents,omitempty"`
	Total     int                          `json:"total,omitempty"`
}

// ResidentHistoryParams identifies a child and an opaque older-page cursor.
// An absent cursor starts at the most recent finalized transcript content.
type ResidentHistoryParams struct {
	ChildID     string `json:"child_id,omitempty"`
	OlderCursor string `json:"older_cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}
