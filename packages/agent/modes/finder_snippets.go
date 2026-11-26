package modes

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
)

// finderSnippetWindow bounds the hits fetched around the hit cursor.
const finderSnippetWindow = 20

type finderSnippetKey struct {
	path string
	seq  int
}

// finderSnippetState caches snippets for the open finder and tracks the
// in-flight fetch. Guarded by Interactive.mu.
type finderSnippetState struct {
	cache  map[finderSnippetKey]string
	req    string // identity of the last requested window
	cancel context.CancelFunc
}

func (s *finderSnippetState) reset() {
	if s.cancel != nil {
		s.cancel()
	}
	*s = finderSnippetState{}
}

// requestFinderSnippets fills snippets for the hits of the selected item
// around the hit cursor: cached texts apply at once, the rest are read on
// a goroutine and applied on the Run loop. A newer request cancels the
// previous fetch; ApplySnippets drops results of an older generation.
func (i *Interactive) requestFinderSnippets(ctx context.Context) {
	i.mu.Lock()
	defer i.mu.Unlock()
	m, index := i.finder, i.finderIndex
	if index == nil || !m.Active() {
		return
	}
	item, ok := m.selected()
	if !ok || len(item.Hits) == 0 {
		return
	}
	lo := max(0, m.hit-finderSnippetWindow/2)
	hi := min(len(item.Hits), lo+finderSnippetWindow)
	req := fmt.Sprintf("%d\x00%s\x00%d", m.gen, item.Key, lo)
	if req == i.finderSnippets.req {
		return
	}
	if i.finderSnippets.cancel != nil {
		i.finderSnippets.cancel()
		i.finderSnippets.cancel = nil
	}
	i.finderSnippets.req = req
	if i.finderSnippets.cache == nil {
		i.finderSnippets.cache = map[finderSnippetKey]string{}
	}
	cached := map[int]string{}
	var refs []sessionindex.HitRef
	slot := map[finderSnippetKey]int{}
	for idx := lo; idx < hi; idx++ {
		ref, ok := item.Hits[idx].Ref.(sessionindex.HitRef)
		if !ok {
			continue
		}
		key := finderSnippetKey{ref.Path, ref.MsgSeq}
		if text, ok := i.finderSnippets.cache[key]; ok {
			cached[idx] = text
		} else if item.Hits[idx].Text == "" {
			refs = append(refs, ref)
			slot[key] = idx
		}
	}
	gen := m.gen
	if len(cached) > 0 {
		m.ApplySnippets(gen, item.Key, cached)
	}
	if len(refs) == 0 {
		return
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	i.finderSnippets.cancel = cancel
	go func() {
		defer cancel()
		snippets, err := index.Snippets(fetchCtx, refs)
		if err != nil || fetchCtx.Err() != nil {
			return
		}
		apply := func() {
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.finder != m || i.finderSnippets.cache == nil {
				return // finder closed or reopened
			}
			texts := make(map[int]string, len(snippets))
			for _, sn := range snippets {
				key := finderSnippetKey{sn.Ref.Path, sn.Ref.MsgSeq}
				i.finderSnippets.cache[key] = sn.Text
				if idx, ok := slot[key]; ok {
					texts[idx] = sn.Text
				}
			}
			if m.ApplySnippets(gen, item.Key, texts) {
				i.invalidate()
			}
		}
		select {
		case i.loopCalls <- apply:
		case <-fetchCtx.Done():
		}
	}()
}
