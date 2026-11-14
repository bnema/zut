package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const codemodeMaxLine = 8 * 1024 * 1024

type codemodeBridge struct {
	inv                   core.NestedInvoker
	code                  string
	calls, errors, denied int
	result                json.RawMessage
	ctxMerge              provider.ToolContext
}

func newCodemodeBridge(inv core.NestedInvoker, code string) *codemodeBridge {
	return &codemodeBridge{inv: inv, code: code}
}

// Accessors are used after serve returns; a bridge serves only one stream.
func (b *codemodeBridge) Counts() (calls, errors, denied int) { return b.calls, b.errors, b.denied }
func (b *codemodeBridge) Result() json.RawMessage             { return bytes.Clone(b.result) }
func (b *codemodeBridge) Context() provider.ToolContext {
	return provider.ToolContext{Reads: slices.Clone(b.ctxMerge.Reads), Discovers: slices.Clone(b.ctxMerge.Discovers), Mutates: slices.Clone(b.ctxMerge.Mutates)}
}

type codemodeReply struct {
	Result any            `json:"result,omitempty"`
	Error  *codemodeError `json:"error,omitempty"`
}
type codemodeError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func codemodeFailure(kind, message string) codemodeReply {
	return codemodeReply{Error: &codemodeError{Kind: kind, Message: message}}
}

// serve processes requests serially and closes both transports on every exit.
func (b *codemodeBridge) serve(ctx context.Context, r io.ReadCloser, w io.WriteCloser) error {
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = r.Close(); _ = w.Close() })
	defer func() {
		if !stop() {
			<-closed
		} else {
			_ = r.Close()
			_ = w.Close()
		}
	}()
	reader := bufio.NewReader(r)
	encoder := json.NewEncoder(w)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var line []byte
		var readErr error
		tooLong := false
		for {
			fragment, err := reader.ReadSlice('\n')
			size := len(fragment)
			if size > 0 && fragment[size-1] == '\n' {
				size--
			}
			if len(line)+size > codemodeMaxLine {
				tooLong = true
				break
			}
			line = append(line, fragment[:size]...)
			if err != bufio.ErrBufferFull {
				readErr = err
				break
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if tooLong {
			// Release a sender still writing the rest of the oversized line.
			_ = r.Close()
			err := encoder.Encode(codemodeFailure("bad_request", "request line exceeds 8 MiB"))
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return nil
		}
		reply := b.handle(ctx, line)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := encoder.Encode(reply); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func codemodeAllowed(name string) bool {
	switch name {
	case "codemode", "subagent", "ask_user_question", "plan", "update_goal", "schedule", "sessions":
		return false
	}
	return true
}

func (b *codemodeBridge) handle(ctx context.Context, line []byte) codemodeReply {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return codemodeFailure("bad_request", "invalid request JSON")
	}
	var p struct {
		Name  string          `json:"name"`
		Args  json.RawMessage `json:"args"`
		Query string          `json:"query"`
		Limit int             `json:"limit"`
		Value json.RawMessage `json:"value"`
	}
	if len(req.Params) != 0 {
		if bytes.Equal(bytes.TrimSpace(req.Params), []byte("null")) || json.Unmarshal(req.Params, &p) != nil {
			return codemodeFailure("bad_request", "params must be an object")
		}
	}
	switch req.Method {
	case "init":
		return codemodeReply{Result: map[string]any{"code": b.code}}
	case "call", "describe":
		if p.Name == "" {
			return codemodeFailure("bad_request", "name is required")
		}
		if !codemodeAllowed(p.Name) {
			return codemodeFailure("not_allowed", fmt.Sprintf("tool %q is not allowed in codemode", p.Name))
		}
		tool, err := b.inv.Tools().Get(p.Name)
		if err != nil {
			return codemodeFailure("unknown_tool", err.Error())
		}
		if req.Method == "describe" {
			return codemodeReply{Result: map[string]any{"name": tool.Name(), "description": tool.Description(), "schema": tool.Schema()}}
		}
		if b.calls >= 200 {
			return codemodeFailure("limit", "codemode permits at most 200 tool calls")
		}
		b.calls++
		out := b.inv.Invoke(ctx, p.Name, p.Args)
		text := codemodeText(out.Result.Content)
		if out.Denied {
			b.denied++
			return codemodeFailure("denied", text)
		}
		if out.Result.IsError {
			b.errors++
		}
		b.mergeContext(out.Result.Context)
		data, err := json.Marshal(out.Result.Data)
		if err != nil {
			data = []byte("null")
		}
		return codemodeReply{Result: map[string]any{"id": out.ID, "is_error": out.Result.IsError, "text": text, "data": json.RawMessage(data)}}
	case "search":
		limit := p.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		type entry struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
		}
		results := []entry{}
		query := strings.ToLower(p.Query)
		for name, tool := range b.inv.Tools() {
			if !codemodeAllowed(name) || !strings.Contains(strings.ToLower(name+" "+tool.Description()), query) {
				continue
			}
			summary, _, _ := strings.Cut(tool.Description(), "\n")
			results = append(results, entry{name, codemodeUTF8Prefix(summary, 160)})
		}
		sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
		if len(results) > limit {
			results = results[:limit]
		}
		return codemodeReply{Result: map[string]any{"results": results}}
	case "result":
		if len(p.Value) == 0 {
			return codemodeFailure("bad_request", "value is required")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, p.Value); err != nil {
			return codemodeFailure("bad_request", "invalid result JSON")
		}
		if compact.Len() > 256*1024 {
			return codemodeFailure("limit", "result exceeds 256 KiB")
		}
		b.result = bytes.Clone(compact.Bytes())
		return codemodeReply{Result: struct{}{}}
	default:
		return codemodeFailure("bad_request", "unknown method")
	}
}

func codemodeUTF8Prefix(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
func codemodeText(content []provider.Content) string {
	const max = 1024 * 1024
	var text strings.Builder
	truncated := false
	for _, c := range content {
		block, ok := c.(provider.TextBlock)
		if !ok {
			continue
		}
		if len(block.Text) > max-text.Len() {
			text.WriteString(codemodeUTF8Prefix(block.Text, max-text.Len()))
			truncated = true
			break
		}
		text.WriteString(block.Text)
	}
	if truncated {
		text.WriteString("\n[codemode: output truncated at 1 MiB]")
	}
	return text.String()
}
func (b *codemodeBridge) mergeContext(c provider.ToolContext) {
	for _, v := range c.Reads {
		if !slices.Contains(b.ctxMerge.Reads, v) {
			b.ctxMerge.Reads = append(b.ctxMerge.Reads, v)
		}
	}
	for _, v := range c.Discovers {
		if !slices.Contains(b.ctxMerge.Discovers, v) {
			b.ctxMerge.Discovers = append(b.ctxMerge.Discovers, v)
		}
	}
	for _, v := range c.Mutates {
		if !slices.Contains(b.ctxMerge.Mutates, v) {
			b.ctxMerge.Mutates = append(b.ctxMerge.Mutates, v)
		}
	}
}
