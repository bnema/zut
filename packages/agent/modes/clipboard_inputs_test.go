package modes

import (
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestFilterInputsAcceptPaste(t *testing.T) {
	paste := tui.Key{Kind: tui.KeyPaste, Paste: "  pasted\n query  "}

	model := &modelDialog{}
	model.HandleKey(paste)
	if model.query.String() != "pasted query" {
		t.Fatalf("model query = %q", model.query.String())
	}

	jump := &jumpDialog{}
	jump.HandleKey(paste)
	if jump.filter != "pasted query" {
		t.Fatalf("jump filter = %q", jump.filter)
	}

	rescue := &rescueDialog{}
	rescue.HandleKey(paste)
	if rescue.query != "pasted query" {
		t.Fatalf("rescue query = %q", rescue.query)
	}

	login := &loginDialog{step: loginStepProvider}
	login.HandleKey(paste)
	if login.providerQuery != "pasted query" {
		t.Fatalf("provider query = %q", login.providerQuery)
	}
}
