package daemonproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
)

// marshalPayload retains concrete content types, including empty reasoning blocks
// and host-only tool routing metadata, without changing durable session JSON.
func marshalPayload(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return enrich(reflect.ValueOf(v), raw)
}
func enrich(v reflect.Value, raw json.RawMessage) (json.RawMessage, error) {
	if !v.IsValid() {
		return raw, nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return raw, nil
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return raw, nil
		}
		return enrich(v.Elem(), raw)
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		return raw, nil
	}
	if v.Type() == reflect.TypeFor[time.Duration]() {
		d := time.Duration(v.Int())
		return json.Marshal(durationValue{MS: d.Milliseconds(), RemainderNS: int64(d % time.Millisecond)})
	}
	if v.CanInterface() {
		if c, ok := v.Interface().(provider.Content); ok {
			var kind string
			switch c.(type) {
			case provider.TextBlock:
				kind = "text"
			case provider.ImageBlock:
				kind = "image"
			case provider.ToolCallBlock:
				kind = "tool_call"
			case provider.ToolResultBlock:
				kind = "tool_result"
			case provider.ReasoningBlock:
				kind = "reasoning"
			default:
				return nil, fmt.Errorf("unsupported content %T", c)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			m["_content"], _ = json.Marshal(kind)
			if tc, ok := c.(provider.ToolCallBlock); ok {
				m["origin"], _ = json.Marshal(tc.Origin)
			}
			if tr, ok := c.(provider.ToolResultBlock); ok {
				r, e := marshalPayload(tr.Content)
				if e != nil {
					return nil, e
				}
				m["content"] = r
				if timing, ok := m["timing"]; ok {
					timing, e = enrich(reflect.ValueOf(tr.Timing), timing)
					if e != nil {
						return nil, e
					}
					m["timing"] = timing
				}
			}
			return json.Marshal(m)
		}
	}
	switch v.Kind() {
	case reflect.Struct:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return raw, nil
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			r, ok := m[key]
			if !ok {
				continue
			}
			r, e := enrich(v.Field(i), r)
			if e != nil {
				return nil, e
			}
			m[key] = r
		}
		return json.Marshal(m)
	case reflect.Map:
		if v.IsNil() || v.Type().Key().Kind() != reflect.String {
			return raw, nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if r, ok := m[key]; ok {
				b, err := enrich(iter.Value(), r)
				if err != nil {
					return nil, err
				}
				m[key] = b
			}
		}
		return json.Marshal(m)
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return raw, nil
		}
		var a []json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return raw, nil
		}
		for i := range a {
			r, e := enrich(v.Index(i), a[i])
			if e != nil {
				return nil, e
			}
			a[i] = r
		}
		return json.Marshal(a)
	}
	return raw, nil
}

// durationValue stores whole milliseconds and a sub-millisecond remainder so
// timing survives round trips without float rounding or nanosecond unit confusion.
type durationValue struct {
	MS          int64 `json:"ms"`
	RemainderNS int64 `json:"remainder_ns,omitempty"`
}

func unmarshalPayload(raw json.RawMessage, out any) error {
	v := reflect.ValueOf(out)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("decode target must be a non-nil pointer")
	}
	return hydrate(raw, v.Elem())
}
func hydrate(raw json.RawMessage, v reflect.Value) error {
	if string(raw) == "null" {
		v.SetZero()
		return nil
	}
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.New(v.Type().Elem()))
		return hydrate(raw, v.Elem())
	}
	if v.Type() == reflect.TypeFor[time.Duration]() {
		var d durationValue
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		const maxMS = int64((1<<63 - 1) / int64(time.Millisecond))
		if d.MS > maxMS || d.MS < -maxMS || d.RemainderNS >= int64(time.Millisecond) || d.RemainderNS <= -int64(time.Millisecond) {
			return fmt.Errorf("invalid duration")
		}
		base := d.MS * int64(time.Millisecond)
		total := base + d.RemainderNS
		if d.RemainderNS > 0 && total < base || d.RemainderNS < 0 && total > base {
			return fmt.Errorf("duration overflow")
		}
		v.SetInt(total)
		return nil
	}
	if v.Type() == reflect.TypeFor[provider.Content]() {
		var h struct {
			Kind string `json:"_content"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return e
		}
		var c provider.Content
		switch h.Kind {
		case "text":
			c = provider.TextBlock{}
		case "image":
			c = provider.ImageBlock{}
		case "tool_call":
			c = provider.ToolCallBlock{}
		case "tool_result":
			c = provider.ToolResultBlock{}
		case "reasoning":
			c = provider.ReasoningBlock{}
		default:
			return fmt.Errorf("unknown content kind %q", h.Kind)
		}
		p := reflect.New(reflect.TypeOf(c))
		if e := hydrate(raw, p.Elem()); e != nil {
			return e
		}
		if h.Kind == "tool_call" {
			var o struct {
				Origin string `json:"origin"`
			}
			if e := json.Unmarshal(raw, &o); e != nil {
				return e
			}
			p.Elem().FieldByName("Origin").SetString(o.Origin)
		}
		v.Set(p.Elem())
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		// Respect standard-library custom JSON values such as time.Time.
		if v.Addr().Type().Implements(reflect.TypeFor[json.Unmarshaler]()) {
			return json.Unmarshal(raw, v.Addr().Interface())
		}
		var m map[string]json.RawMessage
		if e := json.Unmarshal(raw, &m); e != nil {
			return e
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			if r, ok := m[key]; ok {
				if e := hydrate(r, v.Field(i)); e != nil {
					return fmt.Errorf("%s: %w", key, e)
				}
			}
		}
		return nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return json.Unmarshal(raw, v.Addr().Interface())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		v.Set(reflect.MakeMapWithSize(v.Type(), len(m)))
		for k, r := range m {
			entry := reflect.New(v.Type().Elem()).Elem()
			if err := hydrate(r, entry); err != nil {
				return err
			}
			key := reflect.New(v.Type().Key()).Elem()
			key.SetString(k)
			v.SetMapIndex(key, entry)
		}
		return nil
	case reflect.Slice:
		if v.Type() == reflect.TypeFor[json.RawMessage]() || v.Type().Elem().Kind() == reflect.Uint8 {
			return json.Unmarshal(raw, v.Addr().Interface())
		}
		var a []json.RawMessage
		if e := json.Unmarshal(raw, &a); e != nil {
			return e
		}
		v.Set(reflect.MakeSlice(v.Type(), len(a), len(a)))
		for i, r := range a {
			if e := hydrate(r, v.Index(i)); e != nil {
				return e
			}
		}
		return nil
	case reflect.Interface:
		// Untyped tool details must not round integer values through float64.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		return dec.Decode(v.Addr().Interface())
	default:
		return json.Unmarshal(raw, v.Addr().Interface())
	}
}

// EncodeEvent converts a session event into its protocol notification. At is
// not part of the P0 notification contract; sequence and session are retained.
func EncodeEvent(e session.Event) (EventNotification, error) {
	n := EventNotification{Type: "event", Session: e.Session, Seq: e.Seq, Kind: e.Kind}
	var err error
	if e.Kind == session.EvAgent {
		_, n.Data, err = EncodeAgentEvent(e.Agent)
	} else {
		n.Data, err = marshalPayload(e.Data)
	}
	return n, err
}

// DecodeEvent restores typed session payloads from a protocol notification.
func DecodeEvent(n EventNotification) (session.Event, error) {
	e := session.Event{Session: n.Session, Seq: n.Seq, Kind: n.Kind}
	if n.Kind == session.EvAgent {
		var h struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(n.Data, &h); err != nil {
			return e, err
		}
		a, err := DecodeAgentEvent(h.Type, n.Data)
		e.Agent = a
		return e, err
	}
	var p any
	switch n.Kind {
	case session.EvInfo:
		p = &session.InfoEvent{}
	case session.EvQueue:
		p = &session.QueueEvent{}
	case session.EvPending:
		p = &session.PendingEvent{}
	case session.EvPendingResolved:
		p = &session.PendingResolvedEvent{}
	case session.EvDeferred:
		p = &session.DeferredEvent{}
	case session.EvNote:
		p = &session.NoteEvent{}
	case session.EvExtChrome:
		p = &session.ExtChromeEvent{}
	case session.EvExtPanel:
		p = &session.ExtPanelEvent{}
	case session.EvExtAlert:
		p = &session.ExtAlertEvent{}
	case session.EvExtDisplay:
		p = &session.ExtDisplayEvent{}
	case session.EvExtInsert:
		p = &session.ExtInsertEvent{}
	case session.EvPlan:
		p = &session.PlanEvent{}
	case session.EvGoal:
		p = &session.GoalEvent{}
	case session.EvTitle:
		p = &session.TitleEvent{}
	case session.EvCompaction:
		p = &session.CompactionEvent{}
	case session.EvMessagesReplaced:
		p = &session.MessagesReplacedEvent{}
	case session.EvInterSession:
		p = &session.InterSessionEvent{}
	case session.EvAttach:
		p = &session.AttachEvent{}
	case session.EvClosed:
		p = &session.ClosedEvent{}
	default:
		return e, fmt.Errorf("unknown session event %q", n.Kind)
	}
	if err := unmarshalPayload(n.Data, p); err != nil {
		return e, err
	}
	e.Data = reflect.ValueOf(p).Elem().Interface()
	return e, nil
}
