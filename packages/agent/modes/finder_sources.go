package modes

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/sahilm/fuzzy"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
)

const finderSessionLimit = 200

// finderSessionSource is the "Sessions" tab: sessions containing every
// query token somewhere in their text, ranked by title fit, hits and age.
type finderSessionSource struct {
	index *sessionindex.Index
	now   func() time.Time
}

func (*finderSessionSource) ID() string    { return "sessions" }
func (*finderSessionSource) Title() string { return "Sessions" }

func (s *finderSessionSource) Search(ctx context.Context, q finderQuery) (finderResults, error) {
	tokens, here := sessionindex.ParseQuery(q.Text)
	if len(tokens) == 0 {
		return finderResults{}, nil
	}
	res, err := s.index.Query(ctx, finderIndexQuery(q, here, sessionindex.ModeSessions))
	if err != nil {
		return finderResults{}, err
	}
	items := make([]finderItem, 0, len(res.Sessions))
	for _, match := range res.Sessions {
		items = append(items, finderMatchItem(match))
	}
	rankFinderSessions(items, res.Sessions, finderFuzzyText(q.Text), s.now())
	return finderResults{Items: items, Partial: res.Partial}, nil
}

// finderMessageSource is the "Messages" tab: messages containing every
// query token, grouped by session in index order (most recent first).
type finderMessageSource struct {
	index *sessionindex.Index
}

func (*finderMessageSource) ID() string    { return "messages" }
func (*finderMessageSource) Title() string { return "Messages" }

func (s *finderMessageSource) Search(ctx context.Context, q finderQuery) (finderResults, error) {
	tokens, here := sessionindex.ParseQuery(q.Text)
	if len(tokens) == 0 {
		return finderResults{}, nil
	}
	res, err := s.index.Query(ctx, finderIndexQuery(q, here, sessionindex.ModeMessages))
	if err != nil {
		return finderResults{}, err
	}
	items := make([]finderItem, 0, len(res.Sessions))
	for _, match := range res.Sessions {
		items = append(items, finderMatchItem(match))
	}
	return finderResults{Items: items, Partial: res.Partial}, nil
}

func finderIndexQuery(q finderQuery, here bool, mode sessionindex.Mode) sessionindex.Query {
	iq := sessionindex.Query{Text: q.Text, Mode: mode, Limit: finderSessionLimit}
	if here {
		iq.CWD = q.CWD
	}
	return iq
}

// finderDocLabel is "project · title" for a session.
func finderDocLabel(doc sessionindex.SessionDoc) string {
	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = "(untitled)"
	}
	return sessionProject("", doc.CWD) + " · " + title
}

func finderMatchItem(match sessionindex.SessionMatch) finderItem {
	when := match.Doc.Updated
	if when.IsZero() {
		when = match.Doc.Started
	}
	item := finderItem{
		Key:    match.Doc.Path,
		Label:  finderDocLabel(match.Doc),
		Detail: formatRelative(when),
		Count:  match.Count,
		Hits:   make([]finderHit, 0, len(match.Hits)),
	}
	for _, ref := range match.Hits {
		item.Hits = append(item.Hits, finderHit{Ref: ref, Role: ref.Role, Time: ref.Time})
	}
	return item
}

// finderFuzzyText is the query without the @here token, for fuzzy title
// matching.
func finderFuzzyText(text string) string {
	var fields []string
	for _, f := range strings.Fields(text) {
		if !strings.EqualFold(f, "@here") {
			fields = append(fields, f)
		}
	}
	return strings.Join(fields, " ")
}

// rankFinderSessions sorts items (parallel to matches) by
//
//	score = fuzzy + 10*log2(1+hits) + 30/(1+ageDays)
//
// where fuzzy is the sahilm/fuzzy score of the query against the item
// label ("project · title", 0 without a match). A good title match wins,
// then content density, then recency. Ties keep index order.
func rankFinderSessions(items []finderItem, matches []sessionindex.SessionMatch, query string, now time.Time) {
	labels := make([]string, len(items))
	for idx, item := range items {
		labels[idx] = item.Label
	}
	fz := make([]int, len(items))
	if query != "" {
		for _, m := range fuzzy.Find(query, labels) {
			fz[m.Index] = max(m.Score, 0)
		}
	}
	scores := make([]float64, len(items))
	for idx, item := range items {
		age := 0.0
		if updated := matches[idx].Doc.Updated; !updated.IsZero() {
			age = max(now.Sub(updated).Hours()/24, 0)
		}
		scores[idx] = float64(fz[idx]) + 10*math.Log2(1+float64(item.Count)) + 30/(1+age)
	}
	order := make([]int, len(items))
	for idx := range order {
		order[idx] = idx
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	sorted := make([]finderItem, len(items))
	for idx, o := range order {
		sorted[idx] = items[o]
	}
	copy(items, sorted)
}
