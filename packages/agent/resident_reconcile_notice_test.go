package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestResidentReconcileNotice(t *testing.T) {
	corrupt := func(id string) error {
		return fmt.Errorf("resident child %s: %w: orphan tool result", id, subagents.ErrCorruptResidentJournal)
	}
	tests := []struct {
		name string
		errs []error
		want string
	}{
		{name: "none", errs: nil, want: ""},
		{name: "nil only", errs: []error{nil}, want: ""},
		{name: "one corrupt", errs: []error{corrupt("a")}, want: "1 corrupt subagent journal quarantined (quarantined.json)"},
		{name: "many corrupt", errs: []error{corrupt("a"), corrupt("b"), corrupt("c")}, want: "3 corrupt subagent journals quarantined (quarantined.json)"},
		{
			name: "mixed",
			errs: []error{corrupt("a"), errors.New("read root: denied"), errors.New("other")},
			want: "1 corrupt subagent journal quarantined (quarantined.json); 2 subagent children not restored: read root: denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := residentReconcileNotice(tt.errs); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
