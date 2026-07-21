/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// ProgressConfig configures the LoopProgressMonitor. Zero values mean
// "disabled for that signal" so a config with all zeros is equivalent
// to no monitoring at all. The executor populates this from
// Agent.spec.stuckLoopDetection; the empty value (a nil pointer on
// the CRD) yields the DefaultProgressConfig defaults below.
type ProgressConfig struct {
	// RepeatedToolThreshold is how many identical (tool_name, args)
	// calls trigger the duplicate-call signal. Two calls is normal
	// (model reads a file, then re-reads after an edit); the threshold
	// catches the pattern where the model rapid-fires the same call
	// without intermediate progress.
	//
	// Zero disables the signal.
	RepeatedToolThreshold int

	// EditFreeTurnsLimit is the number of consecutive turns without a
	// write_file, str_replace, or submit_result tool call that triggers
	// the edit-free signal. A coder Agent that reads for 15 turns
	// without making a single edit is exploring without converging.
	//
	// Zero disables the signal.
	EditFreeTurnsLimit int

	// ContextSoftCap and ContextHardCap are token-budget guards.
	// Crossing the soft cap nudges the model to wrap up; crossing the
	// hard cap force-terminates regardless of model behavior.
	//
	// Token count is approximated as chars/4 against the wire-mapped
	// transcript (the same heuristic loop.go uses for masking
	// decisions); precise tokenization is not required for an early-
	// warning threshold.
	//
	// Zero disables the signal (caps must be > 0 to be active). When
	// both are set, ContextSoftCap < ContextHardCap must hold;
	// otherwise the monitor treats the config as invalid and disables
	// itself rather than returning a misconfigured-but-active state.
	ContextSoftCap int
	ContextHardCap int
}

// DefaultProgressConfig is the conservative default applied when the
// Agent CR omits stuckLoopDetection. Thresholds are slightly looser
// than the most-aggressive setting we could ship so we don't false-
// trigger on edge cases (a coder genuinely re-reading the same file
// across an edit-verify cycle, for example).
//
// Empirical motivation: the v0.3 batch on 2026-05-26 had Carnice
// rapid-firing the same `git log | grep "449"` 58 times. Threshold=5
// catches it at turn ~6. Edit-free=15 catches a coder that read all
// the way through MaxTurns without ever editing. Caps are sized for
// a 64k-window model with headroom.
var DefaultProgressConfig = ProgressConfig{
	RepeatedToolThreshold: 5,
	EditFreeTurnsLimit:    15,
	ContextSoftCap:        90000,
	ContextHardCap:        140000,
}

// ProgressAction is the recommendation the monitor returns after each
// turn. The loop reads the action and either:
//
//   - Continue: append nothing, run the next turn normally.
//   - Nudge: append a synthetic user message warning the model and
//     suggesting it call submit_result, then run the next turn.
//   - ForceTerminate: synthesize a terminal submit_result with
//     verdict=INCOMPLETE and outcome=STUCK-LOOP-DETECTED, return.
//
// The state machine: Continue -> Nudge (first time a signal fires) ->
// ForceTerminate (next time the same signal still holds). Each signal
// gets one nudge before escalation; once a signal has nudged, that
// signal does not nudge again on the same trajectory.
type ProgressAction int

const (
	// ProgressContinue means no concerning pattern observed.
	ProgressContinue ProgressAction = iota
	// ProgressNudge means a signal fired for the first time. The loop
	// appends the nudge text and gives the model one more turn.
	ProgressNudge
	// ProgressForceTerminate means a previously-nudged signal still
	// holds after the model's chance to recover. The loop synthesizes
	// a submit_result envelope and exits.
	ProgressForceTerminate
)

// ProgressDecision is what Observe returns. Action carries the
// recommendation; Signal names which detector fired (for telemetry);
// Detail is human-readable context the nudge message and force-
// terminate envelope quote so the model and downstream consumers can
// understand why the harness stepped in.
type ProgressDecision struct {
	Action ProgressAction
	Signal string // "RepeatedToolCall" | "EditFreeStreak" | "ContextSoftCap" | "ContextHardCap"
	Detail string // human-readable summary the nudge/force envelope reproduces
}

// LoopProgressMonitor tracks the running state of one Loop.Run call
// and emits ProgressDecisions after each turn. Stateful; one Monitor
// per Run.
//
// All fields are written by Observe and read by Verdict; not safe for
// concurrent use. The loop is single-threaded, so this is fine.
type LoopProgressMonitor struct {
	cfg ProgressConfig

	// History buffers (per signal). All start empty. recentCallHashes
	// is bounded at RepeatedToolThreshold*2 to keep memory small while
	// observing a wide-enough window to detect the pattern.
	recentCallHashes []string
	editFreeStreak   int // turns since last edit/submit
	// groundedFiles is the set of distinct files read_file'd since the last
	// edit. Each distinct file beyond the first raises the effective edit-free
	// limit (grounding-aware guard, #1066): reading real source to ground the
	// next edit (the anti-confab rule, #1062) earns tolerance, while a search
	// loop or a re-read of the same file reads no NEW file and earns none.
	groundedFiles map[string]struct{}
	// lastReadFileKey is the path+range key of the most recent read_file call
	// across all turns. A repeated read_file with the same key is treated as
	// no progress (#1116): it does not add to groundedFiles and does not
	// extend the edit-free budget, preventing a model from burning its
	// forcing-phase turns on identical re-reads.
	lastReadFileKey string
	contextTokens   int // approximate wire token count after most recent turn

	// Nudge state. Once a signal has fired, the next-turn trigger
	// escalates to ForceTerminate. For RepeatedToolCall we track the
	// specific hash that nudged so a model that changes course on the
	// next turn isn't punished for the historical buffer; the
	// nudged-hash recurring even ONCE post-nudge escalates immediately.
	// For the other signals a bool is enough.
	nudgedRepeatedToolHash string // "" means not yet nudged
	nudgedEditFree         bool
	nudgedContextSoft      bool

	// turnSeen is the last turn Observe was called on; protects against
	// out-of-order calls (defensive; the loop calls in order).
	turnSeen int
}

// NewLoopProgressMonitor constructs a monitor with the given config.
// A monitor with an entirely-zero config returns ProgressContinue
// always (effectively disabled) so callers can pass zero values
// safely when the feature is opt-out.
func NewLoopProgressMonitor(cfg ProgressConfig) *LoopProgressMonitor {
	// Validate: if both soft+hard caps are set but soft >= hard, the
	// config is invalid; disable the context signal by clearing both
	// rather than producing meaningless decisions.
	if cfg.ContextSoftCap > 0 && cfg.ContextHardCap > 0 && cfg.ContextSoftCap >= cfg.ContextHardCap {
		cfg.ContextSoftCap = 0
		cfg.ContextHardCap = 0
	}
	return &LoopProgressMonitor{cfg: cfg}
}

// Signal names, used in ProgressDecision.Signal and matched by the loop
// to decide whether the EditFreeStreak forcing function should arm. These
// strings also surface in telemetry and the force-terminate envelope, so
// their values are part of the observable contract.
const (
	signalRepeatedToolCall = "RepeatedToolCall"
	signalEditFreeStreak   = "EditFreeStreak"
	signalContextSoftCap   = "ContextSoftCap"
	signalContextHardCap   = "ContextHardCap"
)

// editProducingTools are the tool names that count as "made progress"
// for the EditFreeStreak signal. submit_result is included so a
// model that immediately submits without editing (legitimate for
// review-only roles or "nothing to fix" cases) does not get
// false-flagged.
var editProducingTools = map[string]struct{}{
	"write_file":    {},
	"str_replace":   {},
	"submit_result": {},
}

// groundingReadBonusCap bounds the edit-free tolerance a model earns by reading
// real source to ground its next edit (#1066). Each distinct file read since the
// last edit, beyond the first, adds one turn to the effective EditFreeTurnsLimit,
// up to this cap, so grounding is rewarded but a pure-read loop still terminates
// at base+cap rather than reading forever.
const groundingReadBonusCap = 8

// editFreeLimit is the effective edit-free trip threshold: the base limit plus
// one turn of tolerance per distinct file the model read to ground its next edit
// since the last edit, beyond the first, capped at groundingReadBonusCap. The
// first distinct read is the file the model is about to edit (base behavior);
// additional distinct files are grounding. A grep/bash search loop or a re-read
// of the same file reads no new file, earns no bonus, and still trips at the base
// limit (#1066).
func (m *LoopProgressMonitor) editFreeLimit() int {
	bonus := len(m.groundedFiles) - 1
	if bonus < 0 {
		bonus = 0
	}
	if bonus > groundingReadBonusCap {
		bonus = groundingReadBonusCap
	}
	return m.cfg.EditFreeTurnsLimit + bonus
}

// readFileKey returns a stable key for a read_file call: "path:offset:limit".
// Offset and limit default to 0 when absent, so a bare read_file("foo.go")
// and read_file("foo.go", offset 1, limit 0) produce the same key.
func readFileKey(arguments string) string {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Path) + ":" + fmt.Sprintf("%d:%d", args.Offset, args.Limit)
}

// explorationTools are the open-ended search tools removed from the
// advertised set during the EditFreeStreak forcing phase. read_file is
// deliberately NOT here: a model recovering from a failed str_replace must
// re-read the target file to get the exact text, and read_file of a
// specific file is part of a legitimate read->edit->verify cycle. The
// pathology the forcing function kills is open-ended *searching*
// (repo-wide grep, bash find, scanning many files), which is what these
// two tools enable.
var explorationTools = map[string]struct{}{
	"grep": {},
	"bash": {},
}

// readOnlyFileTools are dropped from the forcing-phase set ONLY when a
// model profile sets restrictReadsInForcingPhase. By default read_file
// stays (a model recovering from a failed str_replace re-reads the target);
// thrash-prone models that re-read instead of editing opt in to dropping it.
var readOnlyFileTools = map[string]struct{}{
	"read_file": {},
}

// filterForcedEditSchemas returns the advertised tool set for a turn inside
// the EditFreeStreak forcing phase: everything EXCEPT the exploration tools
// (grep, bash), and -- when restrictReads is true -- also except read_file.
// Order is preserved.
func filterForcedEditSchemas(schemas []oai.Tool, restrictReads bool) []oai.Tool {
	out := make([]oai.Tool, 0, len(schemas))
	for _, s := range schemas {
		if _, ok := explorationTools[s.Function.Name]; ok {
			continue
		}
		if restrictReads {
			if _, ok := readOnlyFileTools[s.Function.Name]; ok {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// filterSubmitOnlySchemas returns ONLY the submit_result tool. Used by the
// loop's final-turns convergence guard: in the last few turns before MaxTurns,
// an agent that still has not concluded is advertised submit_result alone, so
// it must produce a terminal verdict instead of silently exhausting MaxTurns
// with no result. This is the reviewer loop's convergence mechanism (reviewers
// have EditFreeStreak disabled by design, since they legitimately read for many
// turns; see progressConfigFromAgent), and a safety net for coders. If
// submit_result is somehow not advertised, the input is returned unchanged.
func filterSubmitOnlySchemas(schemas []oai.Tool) []oai.Tool {
	for _, s := range schemas {
		if s.Function.Name == "submit_result" {
			return []oai.Tool{s}
		}
	}
	return schemas
}

// ForceSubmitMessage is the synthetic user message appended when the loop
// enters the final-turns force-submit window. It tells the model it is out of
// runway and must call submit_result now; turnsLeft is how many turns remain.
func ForceSubmitMessage(turnsLeft int) string {
	plural := "turns"
	if turnsLeft == 1 {
		plural = "turn"
	}
	return fmt.Sprintf(
		"You are almost out of turns (%d %s left) and have not concluded. "+
			"submit_result is now the ONLY tool available: you MUST call it now "+
			"with your verdict, summary, and the required extra fields (for a "+
			"review: reviewOutcome, findings, issueAsk, filesTouched). Do not "+
			"attempt to read or search further; decide on the evidence you have "+
			"and submit. If you genuinely cannot conclude, submit verdict=\"ERROR\" "+
			"(or NO-GO for a review) with a summary of what blocked you.",
		turnsLeft, plural)
}

// Observe records the result of one turn and returns a decision for
// the loop to act on. The transcript argument is the full transcript
// after the turn's tool messages were appended (i.e. what the next
// turn's request would carry on the wire before masking).
//
// Observe must be called exactly once per turn, in order, after the
// turn's tool dispatch has appended to the transcript. Out-of-order
// calls are detected via turnSeen and yield ProgressContinue
// defensively.
func (m *LoopProgressMonitor) Observe(turn int, calls []oai.ToolCall, transcript []oai.Message) ProgressDecision {
	if turn <= m.turnSeen {
		return ProgressDecision{Action: ProgressContinue}
	}
	m.turnSeen = turn

	// 1. Update state from this turn.
	m.recordCalls(calls)
	m.updateEditFreeStreak(calls)
	m.contextTokens = approxTokens(transcript)

	// 2. Evaluate each signal in priority order: hard cap first
	// (deadliest), then soft cap, then edit-free, then repeated tool.
	// A nudged signal escalates to ForceTerminate if it still fires;
	// an un-nudged signal nudges on first fire.

	// 2a. ContextHardCap: no nudge stage; immediate force-terminate.
	if m.cfg.ContextHardCap > 0 && m.contextTokens >= m.cfg.ContextHardCap {
		return ProgressDecision{
			Action: ProgressForceTerminate,
			Signal: signalContextHardCap,
			Detail: fmt.Sprintf(
				"approximate wire-token count %d >= hard cap %d at turn %d",
				m.contextTokens, m.cfg.ContextHardCap, turn),
		}
	}

	// 2b. ContextSoftCap: nudge once, then force-terminate.
	if m.cfg.ContextSoftCap > 0 && m.contextTokens >= m.cfg.ContextSoftCap {
		if m.nudgedContextSoft {
			return ProgressDecision{
				Action: ProgressForceTerminate,
				Signal: signalContextSoftCap,
				Detail: fmt.Sprintf(
					"approximate wire-token count %d >= soft cap %d for second "+
						"consecutive turn (turn %d); model did not call submit_result "+
						"after the prior nudge",
					m.contextTokens, m.cfg.ContextSoftCap, turn),
			}
		}
		m.nudgedContextSoft = true
		return ProgressDecision{
			Action: ProgressNudge,
			Signal: signalContextSoftCap,
			Detail: fmt.Sprintf(
				"approximate wire-token count %d >= soft cap %d at turn %d",
				m.contextTokens, m.cfg.ContextSoftCap, turn),
		}
	}

	// 2c. EditFreeStreak. The threshold is the base limit extended by grounding
	// reads (editFreeLimit, #1066), so reading real source to ground the next
	// edit is not force-terminated like an open-ended search loop.
	if m.cfg.EditFreeTurnsLimit > 0 && m.editFreeStreak >= m.editFreeLimit() {
		if m.nudgedEditFree {
			return ProgressDecision{
				Action: ProgressForceTerminate,
				Signal: signalEditFreeStreak,
				Detail: fmt.Sprintf(
					"no write_file/str_replace/submit_result tool call in %d "+
						"consecutive turns (turn %d); model did not change behavior "+
						"after the prior nudge",
					m.editFreeStreak, turn),
			}
		}
		m.nudgedEditFree = true
		return ProgressDecision{
			Action: ProgressNudge,
			Signal: signalEditFreeStreak,
			Detail: fmt.Sprintf(
				"no write_file/str_replace/submit_result tool call in %d consecutive turns (turn %d)",
				m.editFreeStreak, turn),
		}
	}

	// 2d. RepeatedToolCall.
	//
	// Post-nudge fast path: if we previously nudged on a specific hash
	// and any call in this turn matches that hash, escalate.
	// Otherwise fall through to the threshold check.
	if m.cfg.RepeatedToolThreshold > 0 {
		if m.nudgedRepeatedToolHash != "" {
			for _, tc := range calls {
				h := callHash(tc.Function.Name, tc.Function.Arguments)
				if h == m.nudgedRepeatedToolHash {
					return ProgressDecision{
						Action: ProgressForceTerminate,
						Signal: signalRepeatedToolCall,
						Detail: fmt.Sprintf(
							"%s called again with identical arguments (hash %s) at turn %d after a prior nudge for the same pattern",
							extractToolName(h), h, turn),
					}
				}
			}
		}
		if dup, name, hash := m.findRepeatedCall(); dup {
			m.nudgedRepeatedToolHash = hash
			// Reset the call buffer so the historical duplicates don't
			// keep re-firing on a model that changes course; future
			// recurrence of this specific hash will catch on the
			// fast path above.
			m.recentCallHashes = m.recentCallHashes[:0]
			return ProgressDecision{
				Action: ProgressNudge,
				Signal: signalRepeatedToolCall,
				Detail: fmt.Sprintf(
					"%s called >= %d times with identical arguments (hash %s) at turn %d",
					name, m.cfg.RepeatedToolThreshold, hash, turn),
			}
		}
	}

	return ProgressDecision{Action: ProgressContinue}
}

// recordCalls appends every tool call from this turn to the
// recentCallHashes buffer. Buffer is bounded at RepeatedToolThreshold
// * 2 entries to keep memory small while still observing a window
// large enough to detect the pattern.
func (m *LoopProgressMonitor) recordCalls(calls []oai.ToolCall) {
	if m.cfg.RepeatedToolThreshold <= 0 {
		return
	}
	for _, tc := range calls {
		h := callHash(tc.Function.Name, tc.Function.Arguments)
		m.recentCallHashes = append(m.recentCallHashes, h)
	}
	// Keep the buffer bounded.
	maxLen := m.cfg.RepeatedToolThreshold * 2
	if maxLen < m.cfg.RepeatedToolThreshold+2 {
		maxLen = m.cfg.RepeatedToolThreshold + 2
	}
	if len(m.recentCallHashes) > maxLen {
		m.recentCallHashes = m.recentCallHashes[len(m.recentCallHashes)-maxLen:]
	}
}

// updateEditFreeStreak increments the streak counter when the turn
// contains zero edit-producing calls; resets it when any edit tool
// fires (including submit_result, since that means the model is
// converging). A bash call that writes a workspace file (cat heredoc,
// sed -i, tee, etc.) also counts: models routinely edit through the
// shell instead of the write_file/str_replace tools, and the coder
// commits with `git add -A`, so those changes are real progress.
func (m *LoopProgressMonitor) updateEditFreeStreak(calls []oai.ToolCall) {
	for _, tc := range calls {
		if _, ok := editProducingTools[tc.Function.Name]; ok {
			m.resetEditFreeStreak()
			return
		}
		if tc.Function.Name == "bash" && bashCallMutatesWorkspace(tc.Function.Arguments) {
			m.resetEditFreeStreak()
			return
		}
	}
	// No edit this turn. Record any distinct files the model read so grounding
	// reads raise the effective edit-free limit (see editFreeLimit, #1066).
	// A repeated read_file with the same path+range as the most recent read
	// is treated as no progress (#1116): it does not add to groundedFiles.
	for _, tc := range calls {
		if tc.Function.Name != "read_file" {
			continue
		}
		key := readFileKey(tc.Function.Arguments)
		if key == "" {
			continue
		}
		// Skip repeated reads: same path+range as the last read_file call.
		if key == m.lastReadFileKey {
			continue
		}
		m.lastReadFileKey = key
		if m.groundedFiles == nil {
			m.groundedFiles = make(map[string]struct{})
		}
		m.groundedFiles[key] = struct{}{}
	}
	m.editFreeStreak++
}

// resetEditFreeStreak clears the edit-free streak AND the escalation flag when
// a real edit lands. Resetting nudgedEditFree is what fixes #896: a productive
// edit proves the model is not stuck, so a later edit-free streak (e.g. the
// post-edit go build / go test / git diff verification turns) must earn a fresh
// nudge -> forcing-phase recovery instead of jumping straight to force-terminate
// off a stale nudge from earlier exploration. Without this reset, one early
// explore-nudge poisons the rest of the run: every subsequent streak that hits
// the limit terminates empty-handed even though edits were made and the branch
// was never pushed.
func (m *LoopProgressMonitor) resetEditFreeStreak() {
	m.editFreeStreak = 0
	m.nudgedEditFree = false
	// A landed edit ends the current grounding window; the tolerance earned by
	// reads before it must not carry into a later stuck phase (#1066).
	m.groundedFiles = nil
	m.lastReadFileKey = ""
}

// fileWritingBashTokens are substrings that indicate a bash command
// mutates files in place or writes new ones.
var fileWritingBashTokens = []string{
	"sed -i", "tee ", "mv ", "cp ", "patch ", "dd ", "truncate ", "install ",
	// git apply modifies source files directly without output redirection, so it
	// needs an explicit token — the redirect parser in bashRedirectsToFile won't
	// catch it. Added for #982: models editing via `git apply` were not resetting
	// the EditFreeStreak counter, causing force-terminate mid-edit.
	// NOTE: substring match — also matches `git apply --check/--stat/--numstat`,
	// which are read-only but still safely counted as "edits here" for streak
	// purposes (false positives reset the streak; the only failure mode is a
	// false force-terminate, which this over-match biases away from).
	"git apply",
}

// bashCallMutatesWorkspace parses a bash tool call's JSON arguments and
// reports whether the command likely writes a workspace file. Malformed
// arguments are treated as non-mutating (no reset).
func bashCallMutatesWorkspace(arguments string) bool {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return false
	}
	return bashLikelyMutatesWorkspace(args.Command)
}

// bashLikelyMutatesWorkspace reports whether a bash command probably
// writes or edits a file. It is a heuristic: the EditFreeStreak signal
// is an early warning, not a security control, so the bias is toward
// detecting a write. A false positive merely delays the signal; a false
// negative force-terminates a model that is legitimately editing through
// the shell.
func bashLikelyMutatesWorkspace(command string) bool {
	for _, tok := range fileWritingBashTokens {
		if containsTokenAtWordBoundary(command, tok) {
			return true
		}
	}
	return bashRedirectsToFile(command)
}

// containsTokenAtWordBoundary reports whether tok occurs in command at a word
// boundary: either at the start of the string or immediately after a non-word
// character. Every token in fileWritingBashTokens begins with the command name
// (dd, mv, cp, tee, ...), so a boundary check on the leading char prevents
// matching a token embedded inside an unrelated word. Without it, "git add "
// matches "dd " and "scp x" matches "cp " (#896 secondary).
func containsTokenAtWordBoundary(command, tok string) bool {
	from := 0
	for {
		i := strings.Index(command[from:], tok)
		if i < 0 {
			return false
		}
		abs := from + i
		if abs == 0 || !isWordByte(command[abs-1]) {
			return true
		}
		from = abs + 1
	}
}

// isWordByte reports whether b is an ASCII letter, digit, or underscore, i.e.
// part of a shell word. The boundary check treats anything else (space, &, |,
// ;, /, quotes, start-of-string) as a separator.
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// bashRedirectsToFile reports whether the command contains an output
// redirection ('>' or '>>') whose target is a real file, ignoring
// /dev/{null,stdout,stderr} and file-descriptor duplications (2>&1, >&2).
func bashRedirectsToFile(command string) bool {
	for i := 0; i < len(command); i++ {
		if command[i] != '>' {
			continue
		}
		j := i + 1
		for j < len(command) && command[j] == '>' { // collapse '>>'
			j++
		}
		for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
			j++
		}
		if j >= len(command) || command[j] == '&' {
			// End of string or fd duplication like '>&2'; not a file write.
			continue
		}
		k := j
		for k < len(command) && !strings.ContainsRune(" \t\n;|&)", rune(command[k])) {
			k++
		}
		switch command[j:k] {
		case "/dev/null", "/dev/stdout", "/dev/stderr":
			continue
		default:
			return true
		}
	}
	return false
}

// findRepeatedCall scans the recentCallHashes buffer for any hash
// appearing >= RepeatedToolThreshold times. Returns true plus the
// name/hash of the duplicate so the decision message can reference
// it.
//
// Hashes are stored as the full hash; the name is extracted by
// re-hashing each call name as we walk back through recent calls.
// In practice the bounded buffer keeps this O(buffer-size); the
// fast path on a non-repeated trajectory is a single map lookup.
func (m *LoopProgressMonitor) findRepeatedCall() (dup bool, name, hash string) {
	if len(m.recentCallHashes) < m.cfg.RepeatedToolThreshold {
		return false, "", ""
	}
	counts := make(map[string]int, len(m.recentCallHashes))
	for _, h := range m.recentCallHashes {
		counts[h]++
		if counts[h] >= m.cfg.RepeatedToolThreshold {
			return true, extractToolName(h), h
		}
	}
	return false, "", ""
}

// callHash returns a stable hash of (tool_name, args). The args are
// canonicalized as "name|args" before hashing; small differences
// (whitespace) in the model's JSON output should produce identical
// hashes for semantically identical calls, but we accept that
// trade-off: in practice the model emits identical strings turn after
// turn when stuck (see the v0.3 batch evidence of 58x exact-string
// repeats).
//
// The first 8 chars of the SHA-256 hex digest are the tool-name
// reference embedded as a prefix marker; extractToolName reverses
// this to give the decision message a human-readable name.
func callHash(name, args string) string {
	h := sha256.Sum256([]byte(name + "\x00" + args))
	// Prefix with name (truncated to 16 chars) so extractToolName can
	// recover a readable identifier without us maintaining a separate
	// map. The hash portion ensures uniqueness; the prefix is just for
	// debug ergonomics.
	prefix := name
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return prefix + ":" + hex.EncodeToString(h[:8])
}

// extractToolName recovers the tool name from a callHash output.
// Used for human-readable decision messages.
func extractToolName(hash string) string {
	idx := strings.IndexByte(hash, ':')
	if idx <= 0 {
		return hash
	}
	return hash[:idx]
}

// NudgeMessage returns the synthetic user message the loop appends
// when Observe returns ProgressNudge. The message is a strong
// directive instructing the model to either change approach or call
// submit_result; soft suggestions have shown poor recovery rates on
// confused trajectories in practice.
func NudgeMessage(d ProgressDecision) string {
	var b strings.Builder
	b.WriteString("PROGRESS MONITOR ALERT: ")
	b.WriteString(d.Detail)

	// EditFreeStreak is backed by a forcing function in the loop: the next
	// few turns drop the exploration tools (grep, bash) and the run is
	// force-terminated unless an edit lands. Tell the model so the
	// instruction agrees with the tool set it is about to see; read_file
	// stays available so it can fetch the exact text to edit.
	if d.Signal == signalEditFreeStreak {
		b.WriteString("\n\nYou have been exploring without making any change. Stop searching. " +
			"The grep and bash tools are now DISABLED; you may use read_file to re-read " +
			"the specific file you are changing, but your goal now is to act, not to keep " +
			"looking. Either:\n")
		b.WriteString("  - make the edit the issue calls for (write_file or str_replace; " +
			"read_file first only if you need the exact current text), OR\n")
		b.WriteString("  - if the issue is not fixable or out of scope, call " +
			"submit_result(verdict=\"NO-GO\") with a clear summary.\n\n")
		b.WriteString("You have only a few turns to land a successful edit or submit; " +
			"continuing to only read will be force-terminated as a stuck loop.")
		return b.String()
	}

	b.WriteString("\n\nThis pattern is not making progress. Stop. Either:\n")
	b.WriteString("  - call submit_result(verdict=\"NO-GO\") with a clear summary of what is blocking you, OR\n")
	b.WriteString("  - change your approach entirely (different tool, different arguments, different file).\n\n")
	b.WriteString("Continuing the same pattern will be force-terminated as a stuck loop.")
	return b.String()
}

// ForcedEditReminderMessage is the synthetic user message the loop appends
// on each turn of the EditFreeStreak forcing phase in which the model still
// did not land a successful edit. It restates the constraint and counts
// down the remaining attempts so the model knows the run is about to be
// force-terminated. turnsLeft is how many forced-edit turns remain.
func ForcedEditReminderMessage(turnsLeft int) string {
	plural := "turns"
	if turnsLeft == 1 {
		plural = "turn"
	}
	return fmt.Sprintf(
		"Still no successful edit. grep and bash remain DISABLED. You have %d %s "+
			"left to either land an edit (write_file or str_replace -- use read_file "+
			"first to copy the exact current text if your last str_replace did not "+
			"match) or call submit_result(verdict=\"NO-GO\"). After that the run is "+
			"force-terminated as a stuck loop.",
		turnsLeft, plural)
}

// ForceTerminateEnvelope builds the synthetic submit_result envelope
// the loop emits when Observe returns ProgressForceTerminate. The
// envelope mirrors what the model would emit if it had called
// submit_result(verdict="INCOMPLETE", summary="stuck-loop", ...) so
// downstream consumers (executor, gate, reviewer) see a normal
// terminal shape with a populated extra map.
func ForceTerminateEnvelope(d ProgressDecision, turn int) *ToolResult {
	return &ToolResult{
		Terminal:      true,
		Verdict:       "INCOMPLETE",
		Summary:       "stuck-loop detector intervened: " + d.Signal,
		CommitMessage: "",
		Output: map[string]any{
			"verdict": "INCOMPLETE",
			"summary": "stuck-loop detector intervened: " + d.Signal,
		},
		Extra: map[string]any{
			"outcome":       "STUCK-LOOP-DETECTED",
			"signal":        d.Signal,
			"detail":        d.Detail,
			"terminateTurn": turn,
		},
	}
}
