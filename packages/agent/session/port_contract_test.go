package session

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPayloadJSONRoundTrip(t *testing.T) {
	values := []any{Info{}, Activity{}, GoalSummary{}, PlanStepSummary{}, ResidentSummary{}, Snapshot{}, LiveTurn{}, LiveTool{Args: json.RawMessage(`{}`)}, Input{}, Sender{}, QueuedInput{}, PendingRequest{}, RescueOptions{}, DeferredAction{}, ExtChrome{}, ExtStatus{}, ExtWidget{}, ExtPanel{}, Note{}, SettingsPatch{}, GoalCommand{}, ResidentCommand{}, Tree{}, WorkspaceNode{}, SessionNode{}, CreateSpec{}, ForkSpec{}, Event{}, InfoEvent{}, QueueEvent{}, PendingEvent{}, PendingResolvedEvent{}, DeferredEvent{}, NoteEvent{}, ExtChromeEvent{}, ExtPanelEvent{}, ExtAlertEvent{}, ExtDisplayEvent{}, ExtInsertEvent{}, PlanEvent{}, GoalEvent{}, TitleEvent{}, CompactionEvent{}, MessagesReplacedEvent{}, InterSessionEvent{}, AttachEvent{}, ClosedEvent{}}
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
				t.Fatalf("round trip mismatch: %s", b)
			}
		})
	}
}

var _ Port = (*Controller)(nil)
var _ Handle = (*Controller)(nil)
