package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// runSubagentWorkerMode is the daemon-mode entry point used by every
// subagent-spawned zut subprocess. It's intentionally close in shape to
// runJSONMode but with two key differences:
//
//   - Lifetime: the process stays alive across many user turns. The
//     initial positional task (if any) is the first turn; subsequent
//     turns arrive through the inbox unix socket at args.SubagentWorker.
//
//   - Output: every emitted JSON line is also mirrored verbatim into
//     events.jsonl (see ZUT_SUBAGENT_EVENT_LOG) so a separate zut
//     process can /subagents open this agent and replay its full history
//     even after the parent that spawned us is long gone.
//
// The runner in packages/agent/subagents/runner.go is the only caller in
// production; tests use the stubchild binary under
// packages/agent/subagents/testdata/cmd/stubchild instead of the real model
// loop.
// workerTurnBudget is deliberately independent of the model loop: one
// admitted message turn consumes one current-run and lifetime turn, while
// retries, compaction, and provider/tool loops remain inside that turn.
type workerTurnBudget struct {
	sequence int
	lifetime int
	current  int
}

type sessionPersistenceState struct {
	mu       sync.Mutex
	errValue error
}

func (s *sessionPersistenceState) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.errValue == nil {
		s.errValue = err
	}
	s.mu.Unlock()
}

func (s *sessionPersistenceState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errValue != nil
}

func (s *sessionPersistenceState) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errValue
}

func (b *workerTurnBudget) start(maxTurns int, newRun bool) (step, lifetime, current int, admitted bool) {
	if newRun {
		b.current = 0
	}
	b.sequence++
	if maxTurns > 0 && b.current >= maxTurns {
		return b.sequence, b.lifetime, b.current, false
	}
	b.lifetime++
	b.current++
	return b.sequence, b.lifetime, b.current, true
}

func subagentTurnContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

func runSubagentWorkerMode(ctx context.Context, args Args, version string) (runErr error) {
	if args.SubagentWorker == "" {
		return fmt.Errorf("--subagent-worker requires a socket path")
	}
	if os.Getenv("ZUT_SUBAGENT_CREDENTIAL_STDIN") == "1" {
		var inherited subagents.Credential
		dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
		if err := dec.Decode(&inherited); err != nil {
			return fmt.Errorf("read inherited subagent credential: %w", err)
		}
		if inherited.Value == "" || inherited.Method == "" {
			return fmt.Errorf("read inherited subagent credential: missing value or method")
		}
		args.inheritedCredential = inherited.Value
		args.inheritedAuthMethod = inherited.Method
		args.inheritedAccountID = inherited.AccountID
	}

	cfg, err := loadSubagentWorkerConfig()
	if err != nil {
		return err
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

	ag := r.NewAgent()
	initialAg := ag
	defer func() {
		closeAgentLSP(ag)
		if ag != initialAg {
			closeAgentLSP(initialAg)
		}
	}()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
	sess, err := openOrCreateSession(ctx, args, r, ag, version)
	if err != nil {
		return err
	}
	var sessionPersistence sessionPersistenceState
	if sess != nil {
		var providerName, model string
		sess, ag, providerName, model, err = applyInitialSessionResume(ctx, args, r, extMgr, sess, ag)
		if err != nil {
			return err
		}
		r.Provider, r.Model = providerName, model
		ag.OnToolResult = func(_ string, result core.ToolResult) { persistExtensionToolResult(extMgr, sess, result) }
		// A prompt is acknowledged by the supervisor only after this callback
		// has appended and synced the matching user message. This closes the
		// crash window between accepting a follow-up and writing session.json.
		ag.OnMessageAppended = func(message provider.Message) {
			if err := sess.AppendMessage(message); err != nil {
				sessionPersistence.record(err)
				return
			}
			if err := sess.Sync(); err != nil {
				sessionPersistence.record(err)
			}
		}
		ag.OnUsage = func(usage provider.Usage) {
			if err := sess.AppendUsage(usage, usage); err != nil {
				sessionPersistence.record(err)
			}
		}
		defer func() { runErr = joinSessionCloseError(runErr, sess) }()
	}
	announceSession(extMgr, sess)

	// Resolve retains metadata for catalog models and for valid synthesized
	// local/routed/open-catalog models. The active agent also survives a session
	// resume that rebuilt it for a different provider/model pair.
	contextWindow := ag.ContextWindow

	// Open the inbox listener BEFORE emitting agent_ready so the
	// supervisor can dial through on the very first send. The
	// subagent supervisor's Inbox retries dialing for a
	// short window, but emitting ready first and then listening
	// would still race in tight loops.
	ln, err := subagents.Listen(args.SubagentWorker)
	if err != nil {
		return fmt.Errorf("subagent-worker listen: %w", err)
	}
	defer ln.Close()

	// Event log is owned by the supervisor's runner via stdout, but
	// the daemon also writes a redundant copy here when the runner's
	// pipe is closed (e.g. parent zut exited but the agent is still
	// running headless). The env var is set by the runner; if it's
	// empty we silently skip the second mirror.
	var logMirror *subagents.EventLog
	if path := os.Getenv("ZUT_SUBAGENT_EVENT_LOG"); path != "" {
		logMirror, _ = subagents.OpenEventLog(path)
	}
	if logMirror != nil {
		defer logMirror.Close()
	}

	em := newSubagentEmitter(os.Stdout, logMirror)
	em.setProtocolIdentity(os.Getenv("ZUT_SUBAGENT_AGENT_ID"))
	em.emit("agent.ready", map[string]any{
		"version":           version,
		"cwd":               r.CWD,
		"model":             r.Model,
		"lifetime_turns":    args.SubagentLifetimeTurns,
		"current_run_turns": args.SubagentRunTurns,
		"max_turns":         args.SubagentMaxTurns,
	})

	// Heartbeats make a live-but-detached child distinguishable from a
	// failed turn after supervisor reconnect. The parent still treats
	// unknown event types as forward-compatible.
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		interval := 10 * time.Second
		if raw := os.Getenv("ZUT_SUBAGENT_HEARTBEAT_INTERVAL"); raw != "" {
			if parsed, parseErr := time.ParseDuration(raw); parseErr == nil && parsed > 0 {
				interval = parsed
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				em.emit("agent.heartbeat", map[string]any{"activity": "alive"})
			}
		}
	}()

	// Keep a per-turn cancel so the "cancel" inbox message can interrupt
	// an in-flight turn without tearing down the whole daemon.
	var (
		mu       sync.Mutex
		cancelFn context.CancelFunc
		busyTurn bool
		budget   = workerTurnBudget{
			sequence: initialTurnNumber(os.Getenv("ZUT_SUBAGENT_EVENT_LOG")),
			lifetime: args.SubagentLifetimeTurns,
			current:  args.SubagentRunTurns,
		}
		turnWG         sync.WaitGroup
		closing        bool
		turnPending    bool
		seenCommands   = make(map[string]struct{})
		shutdown       = make(chan struct{})
		shutdownOnce   sync.Once
		shutdownOrigin subagents.ShutdownOrigin
	)
	maxOutputBytes, maxOutputLines := workerOutputLimits()

	runOne := func(prompt string, newRun bool, commandID string) {
		mu.Lock()
		// launchTurn admits the goroutine to turnWG before it starts so
		// shutdown can join it. A shutdown can win the scheduling race
		// after that admission but before this goroutine acquires mu; in
		// that case do not start a new Prompt after waitForActiveTurn has
		// declared the worker closing.
		if closing || busyTurn {
			mu.Unlock()
			em.emit("error", map[string]any{"code": "turn_rejected", "reason": "busy", "command_id": commandID, "message": "worker is busy"})
			return
		}
		busyTurn = true
		turnPending = false
		step, lifetime, currentRun, admitted := budget.start(args.SubagentMaxTurns, newRun)
		if !admitted {
			busyTurn = false
			mu.Unlock()
			turnID := fmt.Sprintf("turn-%d", step)
			errPayload := map[string]any{"code": "turn_rejected", "reason": "max_turns", "command_id": commandID, "message": "maximum subagent turns reached"}
			em.emit("error", errPayload)
			em.emit("turn.result", map[string]any{"status": "failed", "turn_id": turnID, "error": errPayload})
			em.emit("turn.failed", map[string]any{"turn_id": turnID, "error": errPayload})
			em.emit("turn_end", map[string]any{"step": step, "turn_id": turnID, "error": errPayload["message"]})
			// Max-turn rejection ends the current run, but the worker remains
			// alive and can accept a queued follow-up as a fresh run.
			em.emit("agent.idle", map[string]any{
				"turn_id":           turnID,
				"lifetime_turns":    lifetime,
				"current_run_turns": currentRun,
			})
			return
		}
		turnID := fmt.Sprintf("turn-%d", step)
		c, cancel := subagentTurnContext(ctx, args.SubagentTurnTimeout)
		cancelFn = cancel
		mu.Unlock()

		turnStarted := false
		sink := func(ev core.AgentEvent) {
			data := subagentEventData(ev)
			if ev.Type() == "user_message" && !turnStarted {
				// Prompt invokes OnMessageAppended before delivering this sink
				// event, so the session row is durable before the lifecycle
				// acknowledgement is written.
				em.emit(ev.Type(), data)
				if !sessionPersistence.failed() {
					em.emit("turn.started", map[string]any{"step": step, "turn_id": turnID, "command_id": commandID, "lifetime_turns": lifetime, "current_run_turns": currentRun})
					turnStarted = true
				}
				return
			}
			if ev.Type() == "turn_start" {
				// A provider/model-loop turn is nested inside the one delegated
				// message turn admitted above. Mark it on the wire so the
				// supervisor cannot mistake it for a new worker turn.
				data["nested_turn"] = true
			}
			em.emit(ev.Type(), data)
		}

		baseline := workerTranscriptState{
			messages: ag.Messages(),
			cost:     ag.Cost(),
			lastTurn: ag.LastTurnUsage(),
		}
		var persistCompaction func([]provider.Message) error
		if sess != nil {
			// Buffer the checkpoint until the continued turn completes. The final
			// transcript and cumulative usage are then persisted in one JSONL row.
			persistCompaction = func([]provider.Message) error {
				return c.Err()
			}
		}
		recovery, err := promptWithContextRecovery(c, ag, prompt, sink, modes.ContextRecoveryOptions{
			PersistCompaction:    persistCompaction,
			ContextWindow:        contextWindow,
			AutoCompactThreshold: cfg.AutoCompactThreshold,
		})
		output := finalAssistantText(ag.Messages()[recovery.OutputStart:])
		// Message and usage callbacks persist ordinary turns incrementally. A
		// compacted turn still needs one atomic replacement checkpoint so the
		// durable transcript cannot mix generations.
		if recovery.Compacted || ag.OnMessageAppended == nil {
			if persistErr := persistWorkerTranscript(c, ag, sess, recovery.OutputStart, recovery.Compacted, baseline); persistErr != nil {
				if err != nil {
					err = errors.Join(err, persistErr)
				} else {
					err = persistErr
				}
			}
		}
		if sessionErr := sessionPersistence.err(); err == nil && sessionErr != nil {
			err = sessionErr
		}

		resultStatus := "succeeded"
		if err != nil {
			if errors.Is(err, context.Canceled) {
				resultStatus = "canceled"
			} else {
				resultStatus = "failed"
			}
		}
		mu.Lock()
		resultShutdownOrigin := shutdownOrigin.Sanitized()
		mu.Unlock()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			resultShutdownOrigin = subagents.ShutdownOriginDeadline
		case !errors.Is(err, context.Canceled):
			resultShutdownOrigin = ""
		}
		resultPayload := map[string]any{
			"status":  resultStatus,
			"turn_id": turnID,
			"summary": boundedResultSummary(output),
			"output":  boundedResultOutput(output, maxOutputBytes, maxOutputLines),
			"error":   resultErrorPayload(err, resultShutdownOrigin),
		}
		if resultShutdownOrigin != "" {
			resultPayload["shutdown_origin"] = resultShutdownOrigin
		}
		em.emit("turn.result", resultPayload)
		if err != nil {
			em.emit("turn.failed", map[string]any{"turn_id": turnID, "error": resultErrorPayload(err, resultShutdownOrigin)})
		}
		em.emit("turn_end", map[string]any{
			"step":    step,
			"turn_id": turnID,
			"error":   turnErrorMessage(err, resultShutdownOrigin),
		})

		cancel()
		mu.Lock()
		busyTurn = false
		cancelFn = nil
		mu.Unlock()
		// The supervisor treats agent.idle as permission to start a follow-up.
		// Publish it only after the busy gate has opened, otherwise a manager
		// can send a turn that the inbox loop drops as still busy.
		em.emit("agent.idle", map[string]any{"turn_id": turnID, "lifetime_turns": lifetime, "current_run_turns": currentRun})
	}

	launchTurn := func(prompt string, newRun bool, commandID string) bool {
		mu.Lock()
		if closing || busyTurn || turnPending {
			mu.Unlock()
			em.emit("error", map[string]any{"code": "turn_rejected", "reason": "busy", "command_id": commandID, "message": "worker is busy"})
			return false
		}
		// Reserve the worker before launching the goroutine. A second command
		// cannot be accepted in the gap before runOne acquires mu.
		turnPending = true
		turnWG.Add(1)
		mu.Unlock()
		go func() {
			defer turnWG.Done()
			runOne(prompt, newRun, commandID)
		}()
		return true
	}

	waitForActiveTurn := func(cancel bool) {
		mu.Lock()
		closing = true
		activeCancel := cancelFn
		mu.Unlock()
		if cancel && activeCancel != nil {
			activeCancel()
		}
		turnWG.Wait()
	}

	requestShutdown := func(origin subagents.ShutdownOrigin) {
		shutdownOnce.Do(func() {
			mu.Lock()
			closing = true
			shutdownOrigin = origin.Sanitized()
			mu.Unlock()
			close(shutdown)
		})
	}

	// Initial task: run before processing the inbox so the agent
	// "starts working" the moment it boots, matching what users
	// expect from `/subagents new <task>`.
	if args.Prompt != "" {
		launchTurn(args.Prompt, false, "")
	}

	// Inbox loop: one supervisor message at a time. We don't spawn
	// a goroutine per turn because runOne already serialises them
	// via the busyTurn flag; doing the dispatch on the main
	// goroutine keeps the daemon's lifecycle easy to follow.
	for {
		select {
		case <-ctx.Done():
			waitForActiveTurn(true)
			em.emit("agent.exited", map[string]any{"reason": "cancelled"})
			return ctx.Err()
		case <-shutdown:
			// Shutdown is an explicit supervisor stop. Cancel an active
			// turn before joining it so a detached worker cannot remain
			// alive indefinitely while the host waits for its socket to
			// disappear.
			waitForActiveTurn(true)
			em.emit("agent.exited", shutdownExitPayload(shutdownOrigin))
			return nil
		case msg, ok := <-ln.Lines():
			if !ok {
				waitForActiveTurn(true)
				em.emit("agent.exited", map[string]any{"reason": "inbox-closed"})
				return nil
			}
			command, parseErr := subagents.ParseCommand(msg)
			if parseErr != nil {
				em.emit("error", map[string]any{"message": "invalid supervisor command"})
				continue
			}
			switch command.Type {
			case subagents.CommandAgentShutdown:
				var payload subagents.AgentShutdownPayload
				if err := command.DecodePayload(&payload); err != nil {
					em.emit("error", map[string]any{"message": "invalid agent.shutdown payload"})
					continue
				}
				requestShutdown(payload.Origin)
			case subagents.CommandTurnCancel:
				mu.Lock()
				if cancelFn != nil {
					cancelFn()
				}
				mu.Unlock()
			case subagents.CommandAgentPing:
				em.emit("agent.heartbeat", map[string]any{"activity": "alive"})
			case subagents.CommandTurnStart:
				var payload subagents.TurnStartPayload
				if err := command.DecodePayload(&payload); err != nil {
					em.emit("error", map[string]any{"code": "turn_rejected", "reason": "invalid_payload", "command_id": command.MessageID, "message": "invalid turn.start payload"})
					continue
				}
				mu.Lock()
				_, duplicate := seenCommands[command.MessageID]
				mu.Unlock()
				if duplicate && command.MessageID != "" {
					continue
				}
				if launchTurn(payload.Prompt, payload.NewRun, command.MessageID) && command.MessageID != "" {
					mu.Lock()
					seenCommands[command.MessageID] = struct{}{}
					mu.Unlock()
				}
			default:
				// Unknown commands are rejected rather than interpreted as
				// child-originated control requests.
				em.emit("error", map[string]any{"message": "unknown supervisor command"})
			}
		}
	}
}

// subagentEmitter serialises events to stdout and (optionally) to a
// durable log file. Concurrent goroutines call emit so we have to
// hold a mutex around the encoder.
type subagentEmitter struct {
	mu      sync.Mutex
	w       *os.File
	mirror  *subagents.EventLog
	agentID string

	// orphan flips true the first time a stdout write fails (broken
	// pipe — the supervisor died). Until then the mirror stays
	// dormant: the supervisor is the canonical writer to events.jsonl
	// (it parses our stdout and Append()s each event itself). Writing
	// from both sides used to land every event in the log twice,
	// which showed up as a fully-duplicated transcript the next time
	// the agent was reloaded — the exact "why is everything doubled"
	// bug. Once orphaned the mirror takes over so the events still
	// land on disk for the next reload.
	orphan bool
}

func newSubagentEmitter(w *os.File, mirror *subagents.EventLog) *subagentEmitter {
	return &subagentEmitter{w: w, mirror: mirror}
}

func (e *subagentEmitter) setProtocolIdentity(agentID string) {
	e.mu.Lock()
	e.agentID = agentID
	e.mu.Unlock()
}

func (e *subagentEmitter) emit(typ string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	payload := make(map[string]any, len(data))
	for key, value := range data {
		payload[key] = value
	}
	turnID, _ := payload["turn_id"].(string)

	e.mu.Lock()
	defer e.mu.Unlock()

	wireType := canonicalWorkerEvent(typ)
	envelope := subagents.NewEventEnvelope(wireType, e.agentID, turnID, payload)
	line, err := subagents.MarshalJSONL(envelope)

	if !e.orphan && err == nil {
		if _, werr := e.w.Write(line); werr != nil {
			// Supervisor's stdout pipe is gone (parent zut exited but we
			// kept running). Switch to mirror-only mode and preserve this
			// event in the durable log.
			e.orphan = true
		}
	}

	if e.orphan && e.mirror != nil {
		_ = e.mirror.Append(subagents.NewEvent(wireType, payload))
	}
}

func subagentEventData(ev core.AgentEvent) map[string]any {
	data := modes.EventToJSON(ev)
	if turnEnd, ok := ev.(core.EvTurnEnd); ok && provider.IsContextOverflowError(turnEnd.Err) {
		data["error"] = subagentContextLimitMessage
	}
	return data
}

func canonicalWorkerEvent(typ string) string {
	switch typ {
	case "agent_ready":
		return subagents.EventAgentReady
	case "agent_stopped":
		return subagents.EventAgentExited
	case "turn_start":
		return subagents.EventTurnStarted
	case "turn_progress":
		return subagents.EventTurnProgress
	case "tool_call":
		return subagents.EventToolStarted
	case "tool_result":
		return subagents.EventToolFinished
	case "text_delta":
		return subagents.EventMessageDelta
	default:
		return typ
	}
}

const subagentContextLimitMessage = "request exceeds the model context window; narrow the task or reduce gathered context"

func loadSubagentWorkerConfig() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return Config{}, fmt.Errorf("load subagent worker config: %w", err)
	}
	return cfg, nil
}

// workerTranscriptState is restored when an atomic post-compaction checkpoint
// cannot be persisted.
type workerTranscriptState struct {
	messages []provider.Message
	cost     provider.Usage
	lastTurn provider.Usage
}

func persistWorkerTranscript(ctx context.Context, ag *core.Agent, sess *core.Session, from int, compacted bool, baseline workerTranscriptState) error {
	if sess == nil || ag == nil {
		return nil
	}
	if !compacted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := WriteNewTranscript(ag, sess, from); err != nil {
			return fmt.Errorf("persist subagent transcript: %w", err)
		}
		return nil
	}

	rollback := func() {
		ag.SetMessages(baseline.messages)
		ag.SeedCost(baseline.cost)
		ag.SeedLastTurnUsage(baseline.lastTurn)
	}
	if err := ctx.Err(); err != nil {
		rollback()
		return err
	}
	if err := sess.AppendCompactionWithUsage(ag.Messages(), ag.Cost()); err != nil {
		rollback()
		return fmt.Errorf("persist compacted subagent transcript: %w", err)
	}
	return nil
}

// promptWithContextRecovery preserves the worker's existing helper boundary
// while delegating proactive compaction and one-shot overflow recovery to
// modes. The result includes both the output index and compaction state.
func promptWithContextRecovery(ctx context.Context, ag *core.Agent, prompt string, sink func(core.AgentEvent), options modes.ContextRecoveryOptions) (modes.ContextRecoveryResult, error) {
	return modes.PromptWithContextRecovery(ctx, ag, prompt, nil, sink, options)
}

func finalAssistantText(messages []provider.Message) string {
	var out strings.Builder
	for _, message := range messages {
		if message.Role != provider.RoleAssistant {
			continue
		}
		for _, block := range message.Content {
			if text, ok := block.(provider.TextBlock); ok {
				if out.Len() > 0 {
					out.WriteByte('\n')
				}
				out.WriteString(text.Text)
			}
		}
	}
	return out.String()
}

func firstLine(text string) string {
	return strings.Split(strings.TrimSpace(text), "\n")[0]
}

func boundedResultSummary(text string) string {
	return boundedResultOutput(firstLine(text), 4*1024, 1)
}

func shutdownExitPayload(origin subagents.ShutdownOrigin) map[string]any {
	payload := map[string]any{"reason": "shutdown"}
	if origin = origin.Sanitized(); origin != "" {
		payload["origin"] = origin
	}
	return payload
}

func workerOutputLimits() (maxBytes, maxLines int) {
	maxBytes, maxLines = 500_000, 5_000
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ZUT_SUBAGENT_MAX_OUTPUT_BYTES"))); err == nil && value > 0 {
		maxBytes = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ZUT_SUBAGENT_MAX_OUTPUT_LINES"))); err == nil && value > 0 {
		maxLines = value
	}
	return maxBytes, maxLines
}

func boundedResultOutput(text string, maxBytes, maxLines int) string {
	const marker = "...[output truncated]"
	lines := strings.Split(text, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		if maxLines == 1 {
			text = marker
		} else {
			text = strings.Join(append(lines[:maxLines-1], marker), "\n")
		}
	}
	if maxBytes > 0 && len([]byte(text)) > maxBytes {
		if maxBytes <= len(marker) {
			return truncateOutputUTF8(text, maxBytes)
		}
		return truncateOutputUTF8(text, maxBytes-len(marker)) + marker
	}
	return text
}

func truncateOutputUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	data := []byte(value)
	if len(data) <= maxBytes {
		return value
	}
	data = data[:maxBytes]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}

func resultErrorPayload(err error, shutdownOrigin subagents.ShutdownOrigin) map[string]any {
	if err == nil {
		return nil
	}
	if provider.IsContextOverflowError(err) {
		return map[string]any{"code": "context_limit", "message": subagentContextLimitMessage}
	}
	if errors.Is(err, context.DeadlineExceeded) || shutdownOrigin == subagents.ShutdownOriginDeadline {
		return map[string]any{"code": "deadline_exceeded", "message": "subagent turn deadline exceeded; partial output is preserved in the result and history"}
	}
	if errors.Is(err, context.Canceled) {
		if resultErr := subagents.ShutdownResultError(shutdownOrigin); resultErr != nil {
			return map[string]any{"code": resultErr.Code, "message": resultErr.Message}
		}
	}
	return map[string]any{"code": "turn_failed", "message": truncateForLog(err.Error(), 500)}
}

func turnErrorMessage(err error, shutdownOrigin subagents.ShutdownOrigin) string {
	if provider.IsContextOverflowError(err) {
		return subagentContextLimitMessage
	}
	if err != nil {
		if resultErr := subagents.ShutdownResultError(shutdownOrigin); resultErr != nil {
			return resultErr.Message
		}
	}
	return errString(err)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func initialTurnNumber(path string) int {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	events, err := subagents.ReadEventLog(path)
	if err != nil {
		return 0
	}
	maxStep := 0
	for _, ev := range events {
		if !subagents.IsDelegatedTurnStart(ev) {
			continue
		}
		if step, ok := ev.Data["step"].(float64); ok && int(step) > maxStep {
			maxStep = int(step)
		}
		if strings.HasPrefix(ev.TurnID, "turn-") {
			if n, parseErr := strconv.Atoi(strings.TrimPrefix(ev.TurnID, "turn-")); parseErr == nil && n > maxStep {
				maxStep = n
			}
		}
	}
	return maxStep
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return s[:n-3] + "..."
}
