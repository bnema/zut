// Package advisor runs the advisor: a stronger model that watches the
// executor's transcript and intervenes only when it has a concrete reason,
// and that the executor can consult when the user asks.
package advisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// DefaultMaxTokens applies when neither config nor model metadata supply an
// output limit.
const DefaultMaxTokens = 32000

// defaultContextChars bounds the advisor conversation when the model does
// not report a context window.
const defaultContextChars = 400_000

// ErrNoModel is returned by a Target resolver when the advisor is enabled but
// no model has been configured.
var ErrNoModel = errors.New("no advisor model configured; run /advisor provider/model:reasoning")

// Target is a ready advisor endpoint for one request.
type Target struct {
	Client provider.Client
	Model  provider.Model
	// Reasoning is already clamped to Model.
	Reasoning string
	// MaxTokens is the configured output limit; 0 uses the model limit.
	MaxTokens int
}

// Snapshot is the executor context forwarded to the advisor.
type Snapshot struct {
	System   string
	Tools    []provider.Tool
	Messages []provider.Message
}

// Kind is the moment a watcher review happens.
type Kind int

const (
	// KindStep reviews progress in the background after a tool batch.
	KindStep Kind = iota
	// KindChange reviews the executor's first change of a request, before it
	// runs.
	KindChange
	// KindFinish reviews a final answer before the request ends.
	KindFinish
)

// String names the checkpoint, as reported in core.EvAdvisorReview.
func (k Kind) String() string {
	switch k {
	case KindChange:
		return core.AdvisorReviewChange
	case KindFinish:
		return core.AdvisorReviewFinish
	}
	return "step"
}

// Decision is the advisor's choice in a watcher review.
type Decision string

const (
	Silent Decision = "silent"
	Note   Decision = "note"
	Stop   Decision = "stop"
)

// Verdict is a parsed watcher review.
type Verdict struct {
	Decision Decision
	Text     string
}

// maxRememberedNotes bounds the advice carried over a conversation reset.
const maxRememberedNotes = 8

// maxRememberedNoteChars bounds each carried-over note.
const maxRememberedNoteChars = 600

// Reviewer keeps the advisor's own conversation for one executor. The
// executor context is sent once; later requests carry only the transcript
// that is new since the previous one, so the advisor remembers what it saw
// and said, and the stable prefix stays cacheable. Requests are serialized;
// waiting for a turn honors ctx.
type Reviewer struct {
	snapshot func() Snapshot

	// turn is a one-slot semaphore held for a whole request; it guards the
	// conversation fields below.
	turn chan struct{}
	// conv is the advisor conversation: alternating user/assistant turns.
	conv     []provider.Message
	convSize int
	// sent counts executor messages already in conv; anchor is the time of
	// the last one, so a rewritten transcript (compaction, fork, resume)
	// resets the conversation.
	sent   int
	anchor time.Time
	// cacheID keeps one prompt-cache affinity per conversation.
	cacheID string

	mu     sync.Mutex // guards the fields below; never held during a request
	target func(ctx context.Context) (Target, error)
	// consulted holds advisor tool call IDs whose results are the advice
	// itself, already known to the advisor.
	consulted map[string]bool
	// notes is the advice already given (most recent last). It is replayed
	// after a reset so the advisor does not repeat itself.
	notes    []string
	usage    provider.Usage
	hasUsage bool
}

// NewReviewer returns a reviewer resolving its target and executor snapshot
// on every request, so config changes apply without rebuilding it.
func NewReviewer(target func(ctx context.Context) (Target, error), snapshot func() Snapshot) *Reviewer {
	return &Reviewer{target: target, snapshot: snapshot, turn: make(chan struct{}, 1), consulted: map[string]bool{}}
}

// SetTarget replaces the target resolver, for example after the session
// rebuilt its advisor tool.
func (r *Reviewer) SetTarget(target func(ctx context.Context) (Target, error)) {
	r.mu.Lock()
	r.target = target
	r.mu.Unlock()
}

// Review runs a watcher review and returns the advisor's verdict. pending is
// the held call at a KindChange review. started, when set, runs once the
// advisor target resolved and the request is about to be sent. Its usage is
// collected for TakeUsage.
func (r *Reviewer) Review(ctx context.Context, kind Kind, pending *provider.ToolCallBlock, started func()) (Verdict, error) {
	text, usage, err := r.ask(ctx, reviewInstruction(kind, pending), "", started, nil)
	if usage != nil {
		r.addUsage(*usage)
	}
	if err != nil {
		return Verdict{}, err
	}
	v := parseVerdict(text)
	if v.Decision != Silent {
		r.remember(v.Text)
	}
	return v, nil
}

// Consult answers an explicit request for advice from the executor's advisor
// tool call callID. Its usage is returned for the tool result.
func (r *Reviewer) Consult(ctx context.Context, callID string, progress func(string)) (string, *provider.Usage, error) {
	text, usage, err := r.ask(ctx, consultInstruction, callID, nil, progress)
	if err == nil {
		r.mu.Lock()
		if callID != "" {
			r.consulted[callID] = true
		}
		r.mu.Unlock()
		r.remember(text)
	}
	return text, usage, err
}

// TakeUsage returns and clears the usage of watcher reviews.
func (r *Reviewer) TakeUsage() (provider.Usage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.usage, r.hasUsage
	r.usage, r.hasUsage = provider.Usage{}, false
	return u, ok
}

func (r *Reviewer) addUsage(u provider.Usage) {
	r.mu.Lock()
	r.usage = r.usage.Add(u)
	r.hasUsage = true
	r.mu.Unlock()
}

func (r *Reviewer) remember(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	if len(note) > maxRememberedNoteChars {
		note = truncateUTF8(note, maxRememberedNoteChars) + "…"
	}
	r.mu.Lock()
	r.notes = append(r.notes, note)
	if over := len(r.notes) - maxRememberedNotes; over > 0 {
		r.notes = append(r.notes[:0], r.notes[over:]...)
	}
	r.mu.Unlock()
}

// acquire waits for the request slot or ctx.
func (r *Reviewer) acquire(ctx context.Context) error {
	select {
	case r.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("advisor call canceled: %w", ctx.Err())
	}
}

func (r *Reviewer) release() { <-r.turn }

// ask sends the new part of the executor transcript plus instruction and
// returns the advisor's reply. The conversation advances only on success,
// so a failed request is retried with the same delta next time.
func (r *Reviewer) ask(ctx context.Context, instruction, pendingCallID string, started func(), progress func(string)) (string, *provider.Usage, error) {
	if r == nil || r.snapshot == nil {
		return "", nil, errors.New("advisor is not available in this session")
	}
	r.mu.Lock()
	resolve := r.target
	r.mu.Unlock()
	if resolve == nil {
		return "", nil, errors.New("advisor is not available in this session")
	}
	if err := r.acquire(ctx); err != nil {
		return "", nil, err
	}
	defer r.release()

	target, err := resolve(ctx)
	if err != nil {
		return "", nil, err
	}
	if closer, ok := target.Client.(provider.ClientCloser); ok {
		defer func() { _ = closer.Close() }()
	}
	snap := r.snapshot()
	prompt := r.buildPrompt(snap, contextBudget(target.Model), pendingCallID, instruction)
	user := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: prompt}}}

	maxTokens := target.MaxTokens
	if limit := target.Model.MaxOutput; limit > 0 && (maxTokens <= 0 || maxTokens > limit) {
		maxTokens = limit
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	req := provider.Request{
		Model:     target.Model.ID,
		System:    systemPrompt,
		Messages:  append(append([]provider.Message(nil), r.conv...), user),
		MaxTokens: maxTokens,
		Reasoning: target.Reasoning,
		Context:   provider.RequestContext{CacheSessionID: r.cacheID, ThreadID: r.cacheID},
	}
	if started != nil {
		started()
	}
	text, usage, err := streamReply(ctx, target, req, progress)
	if err != nil {
		return "", usage, err
	}

	msgs := snap.Messages
	assistant := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}
	r.conv = append(r.conv, user, assistant)
	r.convSize += len(prompt) + len(text)
	r.sent = len(msgs)
	if len(msgs) > 0 {
		r.anchor = msgs[len(msgs)-1].Time
	}
	return text, usage, nil
}

// buildPrompt renders the next advisor user turn. It resets the
// conversation first when the transcript was rewritten or the turn would
// overflow the budget; a fresh conversation carries the executor context,
// the advisor's earlier notes, and as much recent transcript as fits.
func (r *Reviewer) buildPrompt(snap Snapshot, budget int, pendingCallID, instruction string) string {
	msgs := snap.Messages
	rewritten := r.sent > len(msgs) || (r.sent > 0 && !msgs[r.sent-1].Time.Equal(r.anchor))
	var body string
	if len(r.conv) > 0 && !rewritten {
		body = strings.TrimSpace(core.SerializeAdvisorMessages(r.visible(msgs[r.sent:]), pendingCallID))
		if r.convSize+len(body)+len(instruction) > budget {
			rewritten = true
		}
	}
	if len(r.conv) == 0 || rewritten {
		r.conv, r.convSize, r.sent, r.anchor = nil, 0, 0, time.Time{}
		r.cacheID = newCacheID()
	}

	var prompt strings.Builder
	if len(r.conv) == 0 {
		prompt.WriteString("Executor context:\n\n<context>\n")
		prompt.WriteString(core.SerializeAdvisorContext(snap.System, snap.Tools, msgs))
		prompt.WriteString("</context>\n\n")
		r.mu.Lock()
		notes := append([]string(nil), r.notes...)
		r.mu.Unlock()
		if len(notes) > 0 {
			prompt.WriteString("Advice you already gave in this session (do not repeat it):\n")
			for _, n := range notes {
				prompt.WriteString("- ")
				prompt.WriteString(strings.ReplaceAll(n, "\n", " "))
				prompt.WriteString("\n")
			}
			prompt.WriteString("\n")
		}
		body = strings.TrimSpace(core.SerializeAdvisorMessages(r.visible(msgs), pendingCallID))
		// A transcript too long for the advisor keeps only its most recent
		// part, so the next reviews do not reset and resend it every time.
		if limit := budget/2 - prompt.Len(); limit > 0 && len(body) > limit {
			body = "(earlier transcript omitted)\n" + tailFromLine(body, limit)
		}
	}
	if body != "" {
		prompt.WriteString("New executor transcript:\n\n<transcript>\n")
		prompt.WriteString(body)
		prompt.WriteString("\n</transcript>\n\n")
	} else {
		prompt.WriteString("No new executor activity since your last reply.\n\n")
	}
	prompt.WriteString(instruction)
	return prompt.String()
}

// streamReply sends req and collects the advisor's text and usage.
func streamReply(ctx context.Context, target Target, req provider.Request, progress func(string)) (string, *provider.Usage, error) {
	events, err := target.Client.Stream(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("advisor request failed: %w", err)
	}
	var (
		reply    strings.Builder
		usage    provider.Usage
		hasUsage bool
		doneErr  error
	)
	for ev := range events {
		switch e := ev.(type) {
		case provider.EventTextDelta:
			reply.WriteString(e.Delta)
			if progress != nil && e.Delta != "" {
				progress(e.Delta)
			}
		case provider.EventUsage:
			usage = usage.Add(e.Usage)
			hasUsage = true
		case provider.EventDone:
			doneErr = e.Err
		}
	}
	var usagePtr *provider.Usage
	if hasUsage {
		usage.CostUSD = provider.ComputeCost(target.Model, usage)
		usagePtr = &usage
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", usagePtr, fmt.Errorf("advisor call canceled: %w", ctxErr)
	}
	if doneErr != nil {
		return "", usagePtr, fmt.Errorf("advisor request failed: %w", doneErr)
	}
	text := strings.TrimSpace(reply.String())
	if text == "" {
		return "", usagePtr, errors.New("advisor returned no advice")
	}
	return text, usagePtr, nil
}

// tailFromLine returns about the last limit bytes of s, starting at a line
// boundary when one is available and never inside a UTF-8 sequence.
func tailFromLine(s string, limit int) string {
	cut := len(s) - limit
	if i := strings.IndexByte(s[cut:], '\n'); i >= 0 {
		return s[cut+i+1:]
	}
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// contextBudget is the advisor conversation size, in characters, that fits
// the advisor model with room for its reply.
func contextBudget(model provider.Model) int {
	if model.ContextWindow > 0 {
		// About three characters per token, half the window.
		return model.ContextWindow * 3 / 2
	}
	return defaultContextChars
}

func newCacheID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "advisor-" + hex.EncodeToString(b[:])
}

// visible drops what the advisor already knows: its own notes injected into
// the executor transcript and the results of explicit consultations (both
// kept in notes).
func (r *Reviewer) visible(msgs []provider.Message) []provider.Message {
	r.mu.Lock()
	consulted := make(map[string]bool, len(r.consulted))
	for id := range r.consulted {
		consulted[id] = true
	}
	r.mu.Unlock()
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Meta[core.WatcherNoteMetaKey] == "true" {
			continue
		}
		if m.Role == provider.RoleTool && len(consulted) > 0 {
			kept := make([]provider.Content, 0, len(m.Content))
			for _, c := range m.Content {
				if res, ok := c.(provider.ToolResultBlock); ok && consulted[res.CallID] {
					continue
				}
				kept = append(kept, c)
			}
			m.Content = kept
		}
		out = append(out, m)
	}
	return out
}

// parseVerdict reads the SILENT / NOTE: / STOP: reply format. Free text
// without a marker counts as a note; an empty reply is silent.
func parseVerdict(text string) Verdict {
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)
	switch {
	case text == "" || strings.HasPrefix(upper, "SILENT"):
		return Verdict{Decision: Silent}
	case strings.HasPrefix(upper, "STOP:"):
		return Verdict{Decision: Stop, Text: strings.TrimSpace(text[len("STOP:"):])}
	case strings.HasPrefix(upper, "NOTE:"):
		return Verdict{Decision: Note, Text: strings.TrimSpace(text[len("NOTE:"):])}
	}
	return Verdict{Decision: Note, Text: text}
}
