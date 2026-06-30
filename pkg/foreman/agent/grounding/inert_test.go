package grounding

import (
	"context"
	"strings"
	"testing"
)

func TestDetectInertSymbols(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "git" && strings.HasPrefix(joined, "diff"):
			return "+++ b/internal/controller/model_storage.go\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+\tpvc.Annotations[\"llmkube.io/gpu-taints\"] = taints\n" +
				"+\tpvc.Annotations[\"llmkube.io/wired-key\"] = v\n", nil
		case name == "grep" && strings.Contains(joined, "llmkube.io/gpu-taints"):
			return "internal/controller/model_storage.go:\tpvc.Annotations[\"llmkube.io/gpu-taints\"] = taints\n", nil
		case name == "grep" && strings.Contains(joined, "llmkube.io/wired-key"):
			return "internal/controller/model_storage.go:\tpvc.Annotations[\"llmkube.io/wired-key\"] = v\n" +
				"internal/controller/reader.go:\tif v, ok := o.Annotations[\"llmkube.io/wired-key\"]; ok {\n", nil
		}
		return "", nil
	}
	got := DetectInertSymbols(context.Background(), "/ws", run, "main")
	if len(got) != 1 {
		t.Fatalf("want 1 inert finding (gpu-taints), got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "llmkube.io/gpu-taints") || got[0].Area != "wired-up" {
		t.Errorf("unexpected finding: %+v", got[0])
	}
}

func TestDetectInertSymbols_277Guard(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" {
			return "+++ b/pkg/agent/executor_omlx.go\n@@ -0,0 +1,1 @@\n+\targs = append(args, \"--kv-cache-quant\", n)\n", nil
		}
		return "", nil
	}
	if got := DetectInertSymbols(context.Background(), "/ws", run, "main"); len(got) != 0 {
		t.Fatalf("CLI flag must not be flagged as inert: %v", got)
	}
}
