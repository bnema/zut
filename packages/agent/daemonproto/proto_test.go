package daemonproto

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
)

func TestFrameJSONRoundTrip(t *testing.T) {
	values := []any{HelloRequest{Type: "hello"}, CreateRequest{Type: "create"}, AttachRequest{Type: "attach"}, DetachRequest{Type: "detach"}, CloseRequest{Type: "close"}, ClearRequest{Type: "clear"}, ListRequest{Type: "list"}, TreeRequest{Type: "tree"}, SnapshotRequest{Type: "snapshot"}, PortRequest{Type: "port", Params: json.RawMessage(`{}`)}, DaemonStopRequest{Type: "daemon_stop"}, DaemonStatusRequest{Type: "daemon_status"}, Result{Type: "result", Data: json.RawMessage(`{}`)}, EventNotification{Type: "event", Data: json.RawMessage(`{}`)}, DetachedByOtherNotification{Type: "detached_by_other"}, ClosedNotification{Type: "closed"}, HelloData{ProtocolVersion: 1}, DaemonStatusData{PID: 123}}
	for _, value := range values {
		t.Run(reflect.TypeOf(value).Name(), func(t *testing.T) {
			b, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			out := reflect.New(reflect.TypeOf(value))
			if err = json.Unmarshal(b, out.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value, out.Elem().Interface()) {
				t.Fatalf("mismatch: %s", b)
			}
		})
	}
}

func TestCreateDataPayloadRoundTrip(t *testing.T) {
	in := CreateData{Info: session.Info{ID: "s1", CWD: "/w", Run: session.RunStateIdle}, PreOutput: "pre\n"}
	raw, err := marshalPayload(in)
	if err != nil {
		t.Fatal(err)
	}
	var out CreateData
	if err := unmarshalPayload(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "s1" || out.CWD != "/w" || out.Run != session.RunStateIdle || out.PreOutput != "pre\n" {
		t.Fatalf("round trip = %+v (%s)", out, raw)
	}
	// Older clients decode the same payload as plain Info.
	var info session.Info
	if err := unmarshalPayload(raw, &info); err != nil || info.ID != "s1" {
		t.Fatalf("plain info = %+v, %v", info, err)
	}
	// Older daemons send plain Info: PreOutput stays empty.
	plain, _ := marshalPayload(session.Info{ID: "s2"})
	var old CreateData
	if err := unmarshalPayload(plain, &old); err != nil || old.ID != "s2" || old.PreOutput != "" {
		t.Fatalf("old daemon = %+v, %v", old, err)
	}
}
