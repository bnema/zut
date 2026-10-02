package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeToolArgs(t *testing.T) {
	tab, nl, bell := string(rune(9)), string(rune(10)), string(rune(7))
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: `{}`},
		{name: "valid kept verbatim", in: `{"a":"b\tc"}`, want: `{"a":"b\tc"}`},
		{name: "raw tab in string", in: `{"old":"` + tab + `x"}`, want: `{"old":"\tx"}`},
		{name: "raw newline in string", in: `{"s":"a` + nl + `b"}`, want: `{"s":"a\nb"}`},
		{name: "other control char", in: `{"s":"` + bell + `"}`, want: `{"s":"\u0007"}`},
		{name: "whitespace outside strings untouched", in: "{" + nl + tab + `"s":"` + tab + `"}`, want: "{" + nl + tab + `"s":"\t"}`},
		{name: "escaped quote keeps string state", in: `{"s":"\"` + tab + `"}`, want: `{"s":"\"\t"}`},
		{name: "escaped backslash closes string", in: `{"a":"x\\"` + tab + `}`, want: `{"a":"x\\"` + tab + `}`},
		{name: "multibyte utf-8 kept", in: `{"a":"é` + tab + `"}`, want: `{"a":"é\t"}`},
		{name: "unrepairable", in: `{"s":`, want: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeToolArgs(tt.in)
			if string(got) != tt.want {
				t.Fatalf("normalizeToolArgs(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !json.Valid(got) {
				t.Fatalf("result %q is not valid JSON", got)
			}
		})
	}
}

func TestAnthropicStreamRepairsRawTabInToolArgs(t *testing.T) {
	// partial_json is a JSON string carrying the model's raw argument text;
	// "\\t" below decodes to a real tab byte inside the argument string.
	body := "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"edit\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"old\\\":\\\"\\tx\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	out := make(chan Event, 16)
	go (&anthropicClient{}).runStream(context.Background(), resp, Request{Model: "test"}, out)
	assertToolArgOld(t, out, "\tx")
}

func TestBedrockStreamRepairsRawTabInToolArgs(t *testing.T) {
	// Header-less event-stream frames; the reader takes the event type from
	// the wrapped payload and does not validate CRCs. The `\t` escape in the input
	// field decodes to a real tab byte inside the argument string.
	var body bytes.Buffer
	for _, payload := range []string{
		`{"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"call-1","name":"edit"}}}}`,
		`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"old\":\"\tx\"}"}}}}`,
		`{"contentBlockStop":{"contentBlockIndex":0}}`,
		`{"messageStop":{"stopReason":"tool_use"}}`,
	} {
		var prelude [12]byte
		binary.BigEndian.PutUint32(prelude[0:4], uint32(16+len(payload)))
		body.Write(prelude[:])
		body.WriteString(payload)
		body.Write(make([]byte, 4))
	}
	resp := &http.Response{Body: io.NopCloser(&body)}
	out := make(chan Event, 16)
	go (&bedrockClient{}).runStream(context.Background(), resp, Request{Model: "test"}, out)
	assertToolArgOld(t, out, "\tx")
}

func assertToolArgOld(t *testing.T, out <-chan Event, want string) {
	t.Helper()
	var done EventDone
	for ev := range out {
		if d, ok := ev.(EventDone); ok {
			done = d
		}
	}
	if done.Err != nil {
		t.Fatalf("stream error: %v", done.Err)
	}
	if len(done.Message.Content) != 1 {
		t.Fatalf("content = %#v, want one tool call", done.Message.Content)
	}
	tc, ok := done.Message.Content[0].(ToolCallBlock)
	if !ok {
		t.Fatalf("content[0] = %T, want ToolCallBlock", done.Message.Content[0])
	}
	var args struct{ Old string }
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("arguments %q not valid JSON: %v", tc.Arguments, err)
	}
	if args.Old != want {
		t.Fatalf("old = %q, want %q", args.Old, want)
	}
}
