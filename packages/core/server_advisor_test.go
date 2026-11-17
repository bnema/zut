package core

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestServerAdvisorPassthrough(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "exec", "system", Registry{})
	if err := a.Prompt(context.Background(), "one", nil, nil); err != nil {
		t.Fatal(err)
	}
	if client.lastReq.Advisor != nil {
		t.Fatalf("advisor sent by default: %+v", client.lastReq.Advisor)
	}
	adv := &provider.NativeAdvisor{Model: "adv", MaxTokens: 2048}
	a.SetServerAdvisor(adv)
	adv.Model = "mutated"
	if err := a.Prompt(context.Background(), "two", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := client.lastReq.Advisor; got == nil || *got != (provider.NativeAdvisor{Model: "adv", MaxTokens: 2048}) {
		t.Fatalf("advisor = %+v", got)
	}
	client.lastReq = provider.Request{}
	// The capture client returns no summary text; only the request matters.
	_, _ = a.Compact(context.Background(), 0, nil)
	if client.lastReq.Model == "" {
		t.Fatal("compaction sent no request")
	}
	if client.lastReq.Advisor != nil {
		t.Fatalf("compaction request carried the advisor: %+v", client.lastReq.Advisor)
	}
	a.SetServerAdvisor(nil)
	if err := a.Prompt(context.Background(), "three", nil, nil); err != nil {
		t.Fatal(err)
	}
	if client.lastReq.Advisor != nil {
		t.Fatalf("advisor not cleared: %+v", client.lastReq.Advisor)
	}
}

func TestServerAdvisorFuncEvaluatedPerRequest(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "exec", "system", Registry{})
	a.SetServerAdvisor(&provider.NativeAdvisor{Model: "static"})
	calls := 0
	next := &provider.NativeAdvisor{Model: "dynamic"}
	a.SetServerAdvisorFunc(func() *provider.NativeAdvisor { calls++; return next })
	if err := a.Prompt(context.Background(), "one", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := client.lastReq.Advisor; got == nil || got.Model != "dynamic" {
		t.Fatalf("advisor = %+v", got)
	}
	next = nil
	if err := a.Prompt(context.Background(), "two", nil, nil); err != nil {
		t.Fatal(err)
	}
	if client.lastReq.Advisor != nil {
		t.Fatalf("func returning nil still sent %+v", client.lastReq.Advisor)
	}
	if calls != 2 {
		t.Fatalf("func calls = %d, want 2", calls)
	}
	client.lastReq = provider.Request{}
	_, _ = a.Compact(context.Background(), 0, nil)
	if client.lastReq.Advisor != nil || calls != 2 {
		t.Fatalf("compaction used the advisor func: %+v calls=%d", client.lastReq.Advisor, calls)
	}
	a.SetServerAdvisorFunc(nil)
	if err := a.Prompt(context.Background(), "three", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := client.lastReq.Advisor; got == nil || got.Model != "static" {
		t.Fatalf("cleared func did not fall back to static: %+v", got)
	}
}
