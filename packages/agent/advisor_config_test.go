package agent

import (
	"strings"
	"testing"

	providerpkg "github.com/bnema/zut/packages/provider"
)

func TestParseAdvisorModelSpec(t *testing.T) {
	opus, err := providerpkg.FindModel("anthropic", "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	high := providerpkg.ClampReasoningForModel(opus, "high")
	low := providerpkg.ClampReasoningForModel(opus, "low")
	for _, tc := range []struct {
		name, spec string
		want       AdvisorModelSpec
		errSub     string
	}{
		{name: "plain defaults to high", spec: "anthropic/claude-opus-4-7", want: AdvisorModelSpec{Provider: "anthropic", Model: "claude-opus-4-7", Reasoning: high}},
		{name: "explicit reasoning", spec: "anthropic/claude-opus-4-7:low", want: AdvisorModelSpec{Provider: "anthropic", Model: "claude-opus-4-7", Reasoning: low}},
		{name: "reasoning alias", spec: " anthropic/claude-opus-4-7:hi ", want: AdvisorModelSpec{Provider: "anthropic", Model: "claude-opus-4-7", Reasoning: high}},
		{name: "colon in model id", spec: "amazon-bedrock/amazon.nova-lite-v1:0", want: AdvisorModelSpec{Provider: "amazon-bedrock", Model: "amazon.nova-lite-v1:0", Reasoning: ""}},
		{name: "colon id plus reasoning clamps to none", spec: "amazon-bedrock/amazon.nova-lite-v1:0:high", want: AdvisorModelSpec{Provider: "amazon-bedrock", Model: "amazon.nova-lite-v1:0", Reasoning: ""}},
		{name: "invalid reasoning suffix", spec: "anthropic/claude-opus-4-7:turbo", errSub: `invalid reasoning "turbo"`},
		{name: "empty reasoning suffix", spec: "anthropic/claude-opus-4-7:", errSub: "empty reasoning"},
		{name: "unknown model", spec: "anthropic/nope", errSub: "unknown advisor model"},
		{name: "unknown model with reasoning", spec: "anthropic/nope:high", errSub: "unknown advisor model"},
		{name: "unknown provider", spec: "nope/claude-opus-4-7", errSub: "unknown advisor model"},
		{name: "missing slash", spec: "claude-opus-4-7", errSub: "expected provider/model:reasoning"},
		{name: "empty model", spec: "anthropic/", errSub: "expected provider/model:reasoning"},
		{name: "empty", spec: "", errSub: "expected provider/model:reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAdvisorModelSpec(tc.spec)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err = %v, want containing %q", err, tc.errSub)
				}
				if !strings.Contains(err.Error(), advisorSpecFormat) {
					t.Fatalf("err %q lacks format hint", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got.canonical = ""
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if high == "" {
		t.Fatal("expected claude-opus-4-7 to support reasoning")
	}
}

func TestAdvisorSpecStringRoundTrip(t *testing.T) {
	for _, spec := range []string{"anthropic/claude-opus-4-7:low", "anthropic/claude-opus-4-7:off", "amazon-bedrock/amazon.nova-lite-v1:0"} {
		p, err := ParseAdvisorModelSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseAdvisorModelSpec(p.String())
		if err != nil || again != p {
			t.Fatalf("%s: roundtrip %+v -> %+v (%v)", spec, p, again, err)
		}
	}
}

func TestAdvisorSpecStringForms(t *testing.T) {
	opus, err := providerpkg.FindModel("anthropic", "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	if off := providerpkg.ClampReasoningForModel(opus, ""); off != "" {
		t.Skipf("claude-opus-4-7 cannot disable reasoning (clamps to %q)", off)
	}
	for spec, want := range map[string]string{
		"anthropic/claude-opus-4-7:off":             "anthropic/claude-opus-4-7:off",
		"anthropic/claude-opus-4-7:none":            "anthropic/claude-opus-4-7:off",
		"amazon-bedrock/amazon.nova-lite-v1:0":      "amazon-bedrock/amazon.nova-lite-v1:0",
		"amazon-bedrock/amazon.nova-lite-v1:0:high": "amazon-bedrock/amazon.nova-lite-v1:0",
	} {
		p, err := ParseAdvisorModelSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		if p.String() != want {
			t.Fatalf("%s: String() = %q, want %q", spec, p.String(), want)
		}
	}
}

func TestAdvisorConfigDefaultsAndValidation(t *testing.T) {
	pythonTestEnv(t)
	got := (Config{}).AdvisorSettings()
	if got.Enabled || got.Model != "" || got.Mode != AdvisorModeAuto || got.MaxTokens != 0 {
		t.Fatalf("default = %+v", got)
	}
	cfg := Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7", Mode: "client", MaxTokens: 2048}}
	if s := cfg.AdvisorSettings(); !s.Enabled || s.Mode != AdvisorModeClient || s.MaxTokens != 2048 {
		t.Fatalf("settings = %+v", s)
	}
	for _, bad := range []*AdvisorConfig{{Mode: "native"}, {MaxTokens: -1}} {
		if err := SaveConfig(Config{Advisor: bad}); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "advisor.") {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
}
