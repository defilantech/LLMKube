package grounding

import "testing"

func TestDetectEmpiricalClaims(t *testing.T) {
	added := []AddedLine{
		// the #699 benchmark rows: all three must be detected
		{File: "examples/amd-quickstart/README.md", Line: 154,
			Text: "| Qwen3 30B-A3B (Q8_0) | ~87 tok/s | ~1,200 tok/s | 29/29 | ~18GB |"},
		{File: "examples/amd-quickstart/README.md", Line: 155,
			Text: "| Llama 3.2 3B (Q4_K_M) | ~95 tok/s | ~1,500 tok/s | 29/29 | ~2.5GB |"},
		{File: "examples/amd-quickstart/README.md", Line: 160,
			Text: "These are documented benchmark numbers, validated on gfx1151."},
		// no number with a unit: not a claim
		{File: "examples/amd-quickstart/README.md", Line: 20,
			Text: "Apply the manifest and wait for the pod to become Ready."},
		// version string is not a measurement
		{File: "docs/install.md", Line: 5,
			Text: "Requires LLMKube 0.9.4 or newer."},
		// numbers in Go source are out of scope for the docs claim scan
		{File: "pkg/foreman/agent/loop.go", Line: 300,
			Text: "// retries 3 times, roughly 250ms apart"},
	}
	claims := DetectEmpiricalClaims(added)
	// row 154 (Qwen3): 87 tok/s, 1,200 tok/s, 18GB = 3 claims
	// row 155 (Llama): 95 tok/s, 1,500 tok/s, 2.5GB = 3 claims
	// all other lines contribute zero claims
	if len(claims) != 6 {
		t.Fatalf("got %d claims, want 6 (3 per benchmark row): %+v", len(claims), claims)
	}
	first := claims[0]
	if first.Line != 154 || first.Number != "87" || first.Unit != "tok/s" {
		t.Errorf("claim[0] = %+v, want line 154 number 87 unit tok/s", first)
	}
	// subject tokens must carry the model name so evidence matching can
	// catch misattribution (the #699 failure mode)
	found := false
	for _, s := range first.Subjects {
		if s == "Qwen3" {
			found = true
		}
	}
	if !found {
		t.Errorf("claim[0].Subjects = %v, want to contain Qwen3", first.Subjects)
	}
	// second number+unit match on the same row is its own claim
	second := claims[1]
	if second.Line != 154 || second.Number != "1200" || second.Unit != "tok/s" {
		t.Errorf("claim[1] = %+v, want line 154 number 1200 unit tok/s", second)
	}
	// pins decimal-point preservation: 2.5 must not collide with 25
	sixth := claims[5]
	if sixth.Line != 155 || sixth.Number != "2.5" || sixth.Unit != "GB" {
		t.Errorf("claim[5] = %+v, want line 155 number 2.5 unit GB", sixth)
	}
}
