package session_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
)

func TestDirectoryGeneratedMock(t *testing.T) {
	ctx := context.Background()
	directory := sessionmocks.NewMockDirectory(t)
	want := session.Tree{Workspaces: []session.WorkspaceNode{{RepoRoot: "/workspace"}}}
	directory.EXPECT().Tree(ctx).RunAndReturn(func(context.Context) session.Tree { return want }).Once()
	if got := directory.Tree(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
