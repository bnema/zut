package sessionindex

import (
	"context"
	"os"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestOverview(t *testing.T) {
	root := t.TempDir()
	x := openTestIndex(t, root)
	path := newRootSession(t, root, "/proj/a",
		msg(provider.RoleUser, "first ask"),
		msg(provider.RoleAssistant, "early reply"),
		msg(provider.RoleUser, "second ask"),
		msg(provider.RoleAssistant, "final reply"))
	empty := newRootSession(t, root, "/proj/b", msg(provider.RoleUser, "only ask"))
	mustUpdate(t, x)
	ctx := context.Background()

	ov, ok, err := x.Overview(ctx, path)
	if err != nil || !ok {
		t.Fatalf("overview ok=%v err=%v", ok, err)
	}
	if ov.Doc.Path != path || ov.UserMsgs != 2 || ov.AgentMsgs != 2 || ov.Doc.Provider != "p" || ov.Doc.Model != "m" {
		t.Fatalf("overview = %+v", ov)
	}
	if ov.First == nil || ov.First.Text != "first ask" || ov.Last == nil || ov.Last.Text != "final reply" {
		t.Fatalf("first/last = %+v / %+v", ov.First, ov.Last)
	}
	if ov.Last.Ref.MsgSeq != 3 || ov.Last.Ref.TotalMsgs != 4 {
		t.Fatalf("last ref = %+v", ov.Last.Ref)
	}

	// A plain append reuses the stored model.
	appendSession(t, empty, msg(provider.RoleAssistant, "later"))
	mustUpdate(t, x)
	ov, ok, err = x.Overview(ctx, empty)
	if err != nil || !ok || ov.UserMsgs != 1 || ov.AgentMsgs != 1 || ov.First == nil || ov.Last == nil || ov.Doc.Model != "m" {
		t.Fatalf("appended overview = %+v ok=%v err=%v", ov, ok, err)
	}

	if _, ok, err := x.Overview(ctx, root+"/sessions/x/missing.jsonl"); ok || err != nil {
		t.Fatalf("missing path ok=%v err=%v", ok, err)
	}

	// A rewritten file keeps its counts but drops unreadable texts.
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ov, ok, err = x.Overview(ctx, path)
	if err != nil || !ok || ov.First != nil || ov.Last != nil {
		t.Fatalf("rewritten overview = %+v ok=%v err=%v", ov, ok, err)
	}
}
