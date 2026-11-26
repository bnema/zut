package sessionindex

import (
	"context"

	"github.com/bnema/zut/packages/provider"
)

// Overview summarizes one indexed session.
type Overview struct {
	Doc SessionDoc
	// UserMsgs and AgentMsgs count indexed user and assistant messages.
	UserMsgs, AgentMsgs int
	// First is the first user message and Last the last assistant message.
	// Each is nil when the session has none or its row no longer matches
	// the file (see Snippets).
	First, Last *Snippet
}

// Overview returns the overview of the indexed session at path. ok is
// false when path is not indexed or the index does not exist. It reads the
// session's rows from the index and at most two rows from the session file.
func (x *Index) Overview(ctx context.Context, path string) (ov Overview, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return Overview{}, false, err
	}
	x.mu.Lock()
	seg, err := x.currentLocked()
	if err != nil || seg == nil {
		x.mu.Unlock()
		return Overview{}, false, err
	}
	d := -1
	for i := range seg.docs {
		if seg.docs[i].Path == path && !seg.docs[i].Excluded {
			d = i
			break
		}
	}
	if d < 0 {
		x.mu.Unlock()
		return Overview{}, false, nil
	}
	doc, base := seg.docs[d], seg.first[d]
	rows, err := seg.docRows(d)
	x.mu.Unlock()
	if err != nil {
		return Overview{}, false, err
	}

	ov.Doc = doc.SessionDoc
	first, last := -1, -1
	for idx, r := range rows {
		switch r.Role {
		case provider.RoleUser:
			ov.UserMsgs++
			if first < 0 {
				first = idx
			}
		case provider.RoleAssistant:
			ov.AgentMsgs++
			last = idx
		}
	}
	var refs []HitRef
	for _, idx := range []int{first, last} {
		if idx < 0 {
			continue
		}
		r := rows[idx]
		refs = append(refs, HitRef{
			Path: path, MsgSeq: int(r.MsgID - base), TotalMsgs: doc.TotalMsgs,
			Fingerprint: r.Fingerprint, Role: r.Role, Time: r.Time, Offset: r.Offset, Len: r.Len,
		})
	}
	if len(refs) == 0 {
		return ov, true, nil
	}
	snippets, err := x.Snippets(ctx, refs)
	if err != nil {
		return Overview{}, false, err
	}
	for i := range snippets {
		sn := &snippets[i]
		if sn.Ref.Role == provider.RoleUser {
			ov.First = sn
		} else {
			ov.Last = sn
		}
	}
	return ov, true, nil
}
