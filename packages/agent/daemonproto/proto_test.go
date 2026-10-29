package daemonproto

import (
	"encoding/json"
	"reflect"
	"testing"
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
