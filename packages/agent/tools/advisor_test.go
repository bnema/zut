package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
)

func advisorTestModel() provider.Model {
	return provider.Model{Provider: "test", ID: "advisor-model", MaxOutput: 4096, PriceInput: 10, PriceOutput: 20}
}

func boundAdvisor(target func(context.Context) (AdvisorTarget, error)) *AdvisorTool {
	tool := &AdvisorTool{Target: target}
	tool.Bind(func() AdvisorSnapshot {
		return AdvisorSnapshot{
			System:   "executor system",
			Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fix the parser"}}}},
		}
	})
	return tool
}

func eventStream(events ...provider.Event) <-chan provider.Event {
	ch := make(chan provider.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

func TestAdvisorToolContract(t *testing.T) {
	tool := &AdvisorTool{}
	if tool.Name() != "advisor" || !strings.HasPrefix(tool.Description(), "Consult a stronger reviewer") {
		t.Fatalf("name=%q desc=%q", tool.Name(), tool.Description())
	}
	if string(tool.Schema()) != `{"type":"object","properties":{}}` {
		t.Fatalf("schema=%s", tool.Schema())
	}
	r, err := tool.Execute(context.Background(), nil, nil)
	if err != nil || !r.IsError || !strings.Contains(resultText(r), "not available") {
		t.Fatalf("unbound: %+v %v", r, err)
	}
}

func TestAdvisorToolNoModel(t *testing.T) {
	tool := boundAdvisor(func(context.Context) (AdvisorTarget, error) { return AdvisorTarget{}, ErrNoAdvisorModel })
	r, err := tool.Execute(context.Background(), nil, nil)
	if err != nil || !r.IsError || resultText(r) != "no advisor model configured; run /advisor provider/model:reasoning" || r.Usage != nil {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestAdvisorToolSuccess(t *testing.T) {
	client := providermocks.NewMockClient(t)
	model := advisorTestModel()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		if req.Model != model.ID || req.Reasoning != "high" || req.MaxTokens != 2048 || len(req.Tools) != 0 {
			t.Errorf("request model=%q reasoning=%q max=%d tools=%d", req.Model, req.Reasoning, req.MaxTokens, len(req.Tools))
		}
		if !strings.Contains(req.System, "advisor") || len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
			t.Errorf("request shape: %+v", req)
		}
		body := req.Messages[0].Content[0].(provider.TextBlock).Text
		if !strings.Contains(body, "executor system") || !strings.Contains(body, "fix the parser") {
			t.Errorf("transcript missing: %s", body)
		}
		return eventStream(
			provider.EventTextDelta{Delta: "Check "},
			provider.EventTextDelta{Delta: "the tests."},
			provider.EventUsage{Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100, CostUSD: 99}},
			provider.EventDone{Stop: provider.StopEnd},
		), nil
	})
	tool := boundAdvisor(func(context.Context) (AdvisorTarget, error) {
		return AdvisorTarget{Client: client, Model: model, Reasoning: "high", MaxTokens: 2048}, nil
	})
	var progress []string
	r, err := tool.Execute(context.Background(), nil, func(s string) { progress = append(progress, s) })
	if err != nil || r.IsError || resultText(r) != "Check the tests." {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	if strings.Join(progress, "") != "Check the tests." {
		t.Fatalf("progress=%q", progress)
	}
	want := provider.ComputeCost(model, provider.Usage{InputTokens: 1000, OutputTokens: 100})
	if r.Usage == nil || r.Usage.InputTokens != 1000 || r.Usage.OutputTokens != 100 || r.Usage.CostUSD != want || want == 0 {
		t.Fatalf("usage=%+v want cost %v", r.Usage, want)
	}
}

func TestAdvisorToolMaxTokensDefaultsToModelLimit(t *testing.T) {
	for _, tc := range []struct {
		configured, modelMax, want int
	}{
		{0, 4096, 4096},
		{100000, 4096, 4096},
		{0, 0, defaultAdvisorMaxTokens},
	} {
		client := providermocks.NewMockClient(t)
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
			if req.MaxTokens != tc.want {
				t.Errorf("%+v: MaxTokens=%d", tc, req.MaxTokens)
			}
			return eventStream(provider.EventTextDelta{Delta: "ok"}, provider.EventDone{}), nil
		})
		model := advisorTestModel()
		model.MaxOutput = tc.modelMax
		tool := boundAdvisor(func(context.Context) (AdvisorTarget, error) {
			return AdvisorTarget{Client: client, Model: model, MaxTokens: tc.configured}, nil
		})
		if r, err := tool.Execute(context.Background(), nil, nil); err != nil || r.IsError {
			t.Fatalf("%+v: %+v %v", tc, r, err)
		}
	}
}

func TestAdvisorToolProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stream    func() (<-chan provider.Event, error)
		wantText  string
		wantUsage bool
	}{
		{"setup error", func() (<-chan provider.Event, error) { return nil, errors.New("boom") }, "advisor request failed: boom", false},
		{"stream error keeps usage", func() (<-chan provider.Event, error) {
			return eventStream(provider.EventUsage{Usage: provider.Usage{InputTokens: 5}}, provider.EventDone{Stop: provider.StopError, Err: errors.New("context too long")}), nil
		}, "advisor request failed: context too long", true},
		{"empty advice", func() (<-chan provider.Event, error) { return eventStream(provider.EventDone{}), nil }, "advisor returned no advice", false},
		{"target error", nil, "no credential", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := func(context.Context) (AdvisorTarget, error) {
				return AdvisorTarget{}, errors.New("no credential for advisor provider")
			}
			if tc.stream != nil {
				client := providermocks.NewMockClient(t)
				client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) { return tc.stream() })
				target = func(context.Context) (AdvisorTarget, error) {
					return AdvisorTarget{Client: client, Model: advisorTestModel()}, nil
				}
			}
			r, err := boundAdvisor(target).Execute(context.Background(), nil, nil)
			if err != nil || !r.IsError || !strings.Contains(resultText(r), tc.wantText) || (r.Usage != nil) != tc.wantUsage {
				t.Fatalf("result=%+v err=%v", r, err)
			}
		})
	}
}

func TestAdvisorToolCancellation(t *testing.T) {
	client := providermocks.NewMockClient(t)
	started := make(chan struct{})
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event)
		go func() {
			defer close(ch)
			ch <- provider.EventTextDelta{Delta: "partial"}
			<-ctx.Done()
			ch <- provider.EventDone{Stop: provider.StopError, Err: ctx.Err()}
		}()
		return ch, nil
	})
	tool := boundAdvisor(func(context.Context) (AdvisorTarget, error) {
		return AdvisorTarget{Client: client, Model: advisorTestModel()}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	r, err := tool.Execute(ctx, nil, func(string) {
		select {
		case <-started:
		default:
			close(started)
		}
	})
	if err != nil || !r.IsError || !strings.Contains(resultText(r), "canceled") {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}
