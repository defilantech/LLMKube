package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

func TestContext7Evidence_CollectsMCPToolResults(t *testing.T) {
	tr := []oai.Message{
		{Role: oai.RoleUser, Content: "fix it"},
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c1", Type: "function",
				Function: oai.ToolCallFunction{Name: "mcp/context7/query-docs"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c1", Name: "mcp/context7/query-docs",
			Content: "vllm:request_success_total{finished_reason}"},
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c2", Type: "function", Function: oai.ToolCallFunction{Name: "read_file"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c2", Name: "read_file", Content: "some file"},
	}
	got := context7Evidence(tr)
	want := []string{"vllm:request_success_total{finished_reason}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context7Evidence = %v, want %v", got, want)
	}
}

// Correlation fallback: some backends leave Name empty on the tool message;
// the call name must still be recovered via ToolCallID.
func TestContext7Evidence_CorrelatesByToolCallIDWhenNameEmpty(t *testing.T) {
	tr := []oai.Message{
		{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
			{ID: "c1", Type: "function", Function: oai.ToolCallFunction{Name: "mcp/context7/resolve-library-id"}},
		}},
		{Role: oai.RoleTool, ToolCallID: "c1", Content: "/websites/vllm"},
	}
	if got := context7Evidence(tr); len(got) != 1 || got[0] != "/websites/vllm" {
		t.Fatalf("context7Evidence = %v, want [/websites/vllm]", got)
	}
}

func TestGroundingViolations_FlagsHallucinatedSameNamespaceMetric(t *testing.T) {
	evidence := []string{
		"# HELP vllm:request_success_total ...\nvllm:request_success_total{finished_reason=\"stop\"} 1.0",
	}
	added := []string{
		`- record: llmkube:err`,
		`  expr: rate(vllm:request_failure_total{status_class="5xx"}[5m])`,
	}
	got := groundingViolations(evidence, added)
	if len(got) != 1 {
		t.Fatalf("want 1 violation, got %d: %+v", len(got), got)
	}
	if got[0].Written != "vllm:request_failure_total" || got[0].Namespace != "vllm" {
		t.Fatalf("violation = %+v", got[0])
	}
	// It should offer the retrieved same-namespace name as the alternative.
	found := false
	for _, a := range got[0].RetrievedAlternatives {
		if a == "vllm:request_success_total" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alternatives = %v, want to include vllm:request_success_total", got[0].RetrievedAlternatives)
	}
}

func TestGroundingViolations_CleanWhenWrittenMetricIsInEvidence(t *testing.T) {
	evidence := []string{"vllm:request_success_total{finished_reason}"}
	added := []string{`  expr: rate(vllm:request_success_total[5m])`}
	if got := groundingViolations(evidence, added); len(got) != 0 {
		t.Fatalf("want 0 violations, got %+v", got)
	}
}

func TestGroundingViolations_IgnoresDifferentNamespace(t *testing.T) {
	// context7 was queried about vllm; a go_gc_* metric was never grounded, so
	// it must NOT be flagged even though it is absent from the evidence.
	evidence := []string{"vllm:request_success_total"}
	added := []string{`  expr: go_gc_duration_seconds{quantile="0.5"}`}
	if got := groundingViolations(evidence, added); len(got) != 0 {
		t.Fatalf("want 0 violations for ungrounded namespace, got %+v", got)
	}
}

func TestGroundingViolations_EmptyEvidenceIsNoOp(t *testing.T) {
	if got := groundingViolations(nil, []string{"vllm:request_failure_total"}); len(got) != 0 {
		t.Fatalf("empty evidence must yield no violations, got %+v", got)
	}
}

func TestGroundingViolations_IgnoresHostPortScrapeTargets(t *testing.T) {
	// context7 WAS queried about vllm, but a host:port scrape target in the
	// same namespace must NOT be flagged as a hallucinated metric.
	evidence := []string{"vllm:request_success_total{finished_reason}"}
	added := []string{
		`      - targets: ['vllm:8000']`,
		`      - targets: ['host.docker.internal:9091']`,
	}
	if got := groundingViolations(evidence, added); len(got) != 0 {
		t.Fatalf("host:port targets must not be flagged, got %+v", got)
	}
}

func TestApplyCoderGroundingRail_RecordsViolation(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
		return "diff --git a/x b/x\n+++ b/x\n@@ @@\n+  expr: rate(vllm:request_failure_total[5m])\n", nil
	}
	queryCall := oai.ToolCall{
		ID: "c1", Type: "function",
		Function: oai.ToolCallFunction{Name: "mcp/context7/query-docs"},
	}
	lr := &LoopResult{
		Transcript: []oai.Message{
			{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{queryCall}},
			{
				Role: oai.RoleTool, ToolCallID: "c1", Name: "mcp/context7/query-docs",
				Content: "vllm:request_success_total{finished_reason}",
			},
		},
		Terminal: &ToolResult{Extra: map[string]any{}},
	}
	applyCoderGroundingRail(context.Background(), logr.Discard(), "main", "/ws", lr)
	recs, ok := lr.Terminal.Extra["coderGroundingViolations"].([]map[string]any)
	if !ok || len(recs) != 1 || recs[0]["written"] != "vllm:request_failure_total" {
		t.Fatalf("coderGroundingViolations = %v", lr.Terminal.Extra["coderGroundingViolations"])
	}
}

func TestApplyCoderGroundingRail_NoEvidenceIsNoOp(t *testing.T) {
	lr := &LoopResult{Transcript: nil, Terminal: &ToolResult{Extra: map[string]any{}}}
	applyCoderGroundingRail(context.Background(), logr.Discard(), "main", "/ws", lr)
	if _, present := lr.Terminal.Extra["coderGroundingViolations"]; present {
		t.Fatal("no context7 evidence -> rail must be a no-op")
	}
}

func TestApplyCoderGroundingRailForTask_GatesOnIssueFixKind(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string, _ string, _ ...string) (string, error) {
		return "diff --git a/x b/x\n+++ b/x\n@@ @@\n+  expr: rate(vllm:request_failure_total[5m])\n", nil
	}
	newLoopRes := func() *LoopResult {
		return &LoopResult{
			Transcript: []oai.Message{
				{Role: oai.RoleAssistant, ToolCalls: []oai.ToolCall{
					{ID: "c1", Type: "function", Function: oai.ToolCallFunction{Name: "mcp/context7/query-docs"}},
				}},
				{
					Role: oai.RoleTool, ToolCallID: "c1", Name: "mcp/context7/query-docs",
					Content: "vllm:request_success_total{finished_reason}",
				},
			},
			Terminal: &ToolResult{Extra: map[string]any{}},
		}
	}

	reviewTask := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{Kind: foremanv1alpha1.AgenticTaskKindReview},
	}
	lr := newLoopRes()
	applyCoderGroundingRailForTask(context.Background(), logr.Discard(), reviewTask, "/ws", lr)
	if _, present := lr.Terminal.Extra["coderGroundingViolations"]; present {
		t.Fatal("kind != issue-fix -> rail must be a no-op")
	}

	issueFixTask := &foremanv1alpha1.AgenticTask{
		Spec: foremanv1alpha1.AgenticTaskSpec{Kind: foremanv1alpha1.AgenticTaskKindIssueFix},
	}
	lr = newLoopRes()
	applyCoderGroundingRailForTask(context.Background(), logr.Discard(), issueFixTask, "/ws", lr)
	recs, ok := lr.Terminal.Extra["coderGroundingViolations"].([]map[string]any)
	if !ok || len(recs) != 1 || recs[0]["written"] != "vllm:request_failure_total" {
		t.Fatalf("issue-fix kind should apply the rail; got %v", lr.Terminal.Extra["coderGroundingViolations"])
	}
}

func TestGroundingViolations_Issue409Fixture(t *testing.T) {
	// Real 2026-07-08 #409 run: context7 returned vLLM's /metrics output; the
	// coder wrote the hallucinated failure metric instead of the retrieved
	// success metric. The rail must flag exactly the hallucinated name.
	evidence := []string{
		`# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="meta-llama/Llama-3.1-8B-Instruct"} 8.0
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{finished_reason="stop",model_name="meta-llama/Llama-3.1-8B-Instruct"} 1.0`,
	}
	added := []string{
		`  - record: llmkube:inference:error_rate:5m`,
		`    expr: rate(vllm:request_failure_total{status_class="5xx"}[5m])`,
	}
	got := groundingViolations(evidence, added)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 violation, got %d: %+v", len(got), got)
	}
	if got[0].Written != "vllm:request_failure_total" {
		t.Fatalf("violation.Written = %q, want vllm:request_failure_total", got[0].Written)
	}
	hasAlt := false
	for _, a := range got[0].RetrievedAlternatives {
		if a == "vllm:request_success_total" {
			hasAlt = true
		}
	}
	if !hasAlt {
		t.Fatalf("RetrievedAlternatives = %v, want to include vllm:request_success_total", got[0].RetrievedAlternatives)
	}
}
