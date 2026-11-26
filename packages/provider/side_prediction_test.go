package provider

import (
	"context"
	"testing"
)

type sidePredictionPlainClient struct{}

func (sidePredictionPlainClient) Name() string { return "plain" }
func (sidePredictionPlainClient) Stream(context.Context, Request) (<-chan Event, error) {
	return nil, nil
}

func TestSupportsSidePrediction(t *testing.T) {
	copilot := &copilotClient{}
	cases := map[string]struct {
		client Client
		want   bool
	}{
		"nil":                {nil, false},
		"default":            {sidePredictionPlainClient{}, true},
		"copilot":            {copilot, false},
		"refreshing copilot": {NewRefreshingClient(copilot, nil, nil), false},
		"renamed copilot":    {&renamedClient{inner: copilot, name: "x"}, false},
		"renamed default":    {&renamedClient{inner: sidePredictionPlainClient{}, name: "x"}, true},
	}
	for name, tc := range cases {
		if got := SupportsSidePrediction(tc.client); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
