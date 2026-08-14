package agent

import (
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
)

func TestBuildToolRegistryGrepSelectionAndDefaults(t *testing.T) {
	cwd := t.TempDir()

	defaultRegistry := buildToolRegistry(Args{}, cwd, nil, false, false, false)
	if _, ok := defaultRegistry["grep"]; !ok {
		t.Fatal("default registry does not contain grep")
	}
	defaultSummaries := toolSummaries(defaultRegistry, Args{})
	grepIndex := -1
	for i, summary := range defaultSummaries {
		if summary.Name == "grep" {
			grepIndex = i
			break
		}
	}
	if grepIndex < 0 {
		t.Fatal("default tool summaries do not contain grep")
	}
	if grepIndex == 0 || defaultSummaries[grepIndex-1].Name != "edit" {
		t.Fatalf("grep summary order has predecessor %q, want edit", defaultSummaries[grepIndex-1].Name)
	}

	selected := buildToolRegistry(Args{Tools: []string{"grep"}}, cwd, nil, false, false, false)
	if len(selected) != 1 {
		t.Fatalf("selected registry has %d tools, want one", len(selected))
	}
	if _, ok := selected["grep"].(*tools.GrepTool); !ok {
		t.Fatalf("selected grep has type %T", selected["grep"])
	}
	if summaries := toolSummaries(selected, Args{Tools: []string{"grep"}}); len(summaries) != 1 || summaries[0].Name != "grep" {
		t.Fatalf("selected summaries = %#v, want only grep", summaries)
	}

	noTools := buildToolRegistry(Args{NoTools: true}, cwd, nil, false, false, false)
	if len(noTools) != 0 {
		t.Fatalf("no-tools registry has %d tools", len(noTools))
	}
	if summaries := toolSummaries(noTools, Args{NoTools: true}); len(summaries) != 0 {
		t.Fatalf("no-tools summaries = %#v", summaries)
	}
}
