package agent

import (
	"context"
	"regexp"
	"strings"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// metricIdentRe matches Prometheus/vLLM metric-name-shaped identifiers:
// a namespace, a colon, then the metric path (e.g. vllm:request_failure_total,
// llmkube:inference:ttft_seconds:p95_5m). v1 checks exactly this class -- the
// tokens most prone to hallucination and the ones that failed on #409/#850.
var metricIdentRe = regexp.MustCompile(`[a-z_][a-z0-9_]*:[a-z0-9_:]+`)

// metricIdents returns the metric-name-shaped identifiers in s, EXCLUDING
// host:port network addresses (whose path after the first colon is all digits,
// e.g. "vllm:8000") -- those are not Prometheus metrics and must not be flagged.
func metricIdents(s string) []string {
	var out []string
	for _, m := range metricIdentRe.FindAllString(s, -1) {
		i := strings.IndexByte(m, ':')
		if i < 0 {
			continue
		}
		path := m[i+1:]
		if path == "" || isAllDigits(path) {
			continue // host:port, not a metric
		}
		out = append(out, m)
	}
	return out
}

// isAllDigits returns true if s contains only ASCII digit characters.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// groundingViolation is one written external identifier that contradicts the
// retrieved docs: its namespace was the subject of a context7 lookup this run,
// yet the identifier itself does not appear anywhere in the retrieved evidence.
type groundingViolation struct {
	Written               string
	Namespace             string
	RetrievedAlternatives []string
}

// context7Evidence gathers the content of every mcp/* tool result in the
// transcript into an evidence corpus. The tool message's Name is the function
// name; when a backend leaves Name empty we recover it from the assistant's
// tool_call by ToolCallID.
func context7Evidence(transcript []oai.Message) []string {
	callName := make(map[string]string)
	for _, m := range transcript {
		if m.Role == oai.RoleAssistant {
			for _, tc := range m.ToolCalls {
				callName[tc.ID] = tc.Function.Name
			}
		}
	}
	var ev []string
	for _, m := range transcript {
		if m.Role != oai.RoleTool {
			continue
		}
		name := m.Name
		if name == "" {
			name = callName[m.ToolCallID]
		}
		if strings.HasPrefix(name, "mcp/") {
			ev = append(ev, m.Content)
		}
	}
	return ev
}

func namespaceOf(ident string) string {
	if i := strings.IndexByte(ident, ':'); i > 0 {
		return ident[:i]
	}
	return ""
}

// groundingViolations flags each metric-shaped identifier in addedLines whose
// namespace appears in the evidence (so context7 was queried about that domain)
// but whose full identifier does NOT appear verbatim in the evidence -- a
// likely hallucination the coder wrote despite the docs it fetched.
func groundingViolations(evidence []string, addedLines []string) []groundingViolation {
	if len(evidence) == 0 {
		return nil
	}
	corpus := strings.Join(evidence, "\n")

	// All metric identifiers present in the evidence, grouped by namespace.
	retrievedByNS := make(map[string][]string)
	retrievedSet := make(map[string]bool)
	for _, e := range metricIdents(corpus) {
		if retrievedSet[e] {
			continue
		}
		retrievedSet[e] = true
		if ns := namespaceOf(e); ns != "" {
			retrievedByNS[ns] = append(retrievedByNS[ns], e)
		}
	}

	var out []groundingViolation
	seen := make(map[string]bool)
	for _, line := range addedLines {
		for _, w := range metricIdents(line) {
			if seen[w] {
				continue
			}
			ns := namespaceOf(w)
			// Only check a namespace context7 was actually queried about.
			if len(retrievedByNS[ns]) == 0 {
				continue
			}
			// Grounded if the exact identifier is in the evidence.
			if strings.Contains(corpus, w) {
				continue
			}
			seen[w] = true
			out = append(out, groundingViolation{
				Written:               w,
				Namespace:             ns,
				RetrievedAlternatives: retrievedByNS[ns],
			})
		}
	}
	return out
}

// applyCoderGroundingRail records, on the coder's terminal result, any
// external metric identifier the coder WROTE that contradicts the context7
// docs it retrieved this run (namespace was queried, but the identifier is
// absent from the docs). Non-blocking (v1): it surfaces violations under
// Extra["coderGroundingViolations"] and logs them; the verdict is unchanged.
// No-op unless there is context7 evidence AND added diff lines. Degrades
// open: never returns an error, never mutates the verdict.
func applyCoderGroundingRail(ctx context.Context, log logr.Logger, base, workspace string, loopRes *LoopResult) {
	if loopRes == nil || loopRes.Terminal == nil {
		return
	}
	evidence := context7Evidence(loopRes.Transcript)
	if len(evidence) == 0 {
		return
	}
	added := addedDiffLines(ctx, workspace, base, execCommandRunner)
	if len(added) == 0 {
		return
	}
	violations := groundingViolations(evidence, added)
	if len(violations) == 0 {
		return
	}
	recs := make([]map[string]any, 0, len(violations))
	for _, v := range violations {
		log.Info("coder grounding: wrote an identifier absent from the retrieved docs",
			"written", v.Written, "namespace", v.Namespace, "retrievedAlternatives", v.RetrievedAlternatives)
		recs = append(recs, map[string]any{
			"written":               v.Written,
			"namespace":             v.Namespace,
			"retrievedAlternatives": v.RetrievedAlternatives,
		})
	}
	if loopRes.Terminal.Extra == nil {
		loopRes.Terminal.Extra = map[string]any{}
	}
	loopRes.Terminal.Extra["coderGroundingViolations"] = recs
}

// applyCoderGroundingRailForTask gates applyCoderGroundingRail to issue-fix
// tasks and resolves the base branch. Extracted out of runLLMPath so the
// call site there is a single statement rather than a branch, keeping
// runLLMPath's cyclomatic complexity budget untouched.
func applyCoderGroundingRailForTask(
	ctx context.Context, log logr.Logger, task *foremanv1alpha1.AgenticTask, workspace string, loopRes *LoopResult,
) {
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		return
	}
	applyCoderGroundingRail(ctx, log, baseBranchOrDefault(task.Spec.Payload.BaseBranch), workspace, loopRes)
}
