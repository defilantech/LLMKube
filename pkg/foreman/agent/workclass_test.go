package agent

import "testing"

func TestClassifyFile(t *testing.T) {
	cases := []struct {
		path string
		want workClass
	}{
		{".github/workflows/release-please.yml", workClassCIPolicy},
		{".github/actions/setup/action.yml", workClassCIPolicy},
		{".goreleaser.yaml", workClassReleasePolicy},
		{"hack/publish-homebrew-formula.sh", workClassPackaging},
		{"Formula/llmkube.rb", workClassPackaging},
		{"charts/llmkube/templates/deployment.yaml", workClassPackaging},
		{"Dockerfile.coder", workClassPackaging},
		{"docs/proposals/697-amd-vulkan-runtime-image.md", workClassDocs},
		{"examples/amd-quickstart/README.md", workClassDocs},
		{"config/foreman/agents/gate.yaml", workClassConfig},
		{"pkg/foreman/agent/loop.go", workClassCodeFix},
		{"internal/controller/model_controller_test.go", workClassCodeFix},
	}
	for _, c := range cases {
		if got := classifyFile(c.path); got != c.want {
			t.Errorf("classifyFile(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestClassifyFootprint(t *testing.T) {
	cases := []struct {
		name    string
		changed map[string]int
		want    workClass
	}{
		{"all workflow yaml is ci-policy", map[string]int{
			".github/workflows/release-please.yml": 40}, workClassCIPolicy},
		{"docs dominate at 70 percent", map[string]int{
			"examples/amd-quickstart/README.md": 70,
			"pkg/cli/deploy.go":                 30}, workClassDocs},
		{"no dominant class is mixed", map[string]int{
			"pkg/foreman/agent/loop.go":            50,
			".github/workflows/release-please.yml": 50}, workClassMixed},
		{"all-zero counts are mixed not random", map[string]int{
			".github/workflows/x.yml": 0,
			"pkg/a.go":                0}, workClassMixed},
		{"empty diff is code-fix", map[string]int{}, workClassCodeFix},
	}
	for _, c := range cases {
		if got := classifyFootprint(c.changed); got != c.want {
			t.Errorf("%s: classifyFootprint = %q, want %q", c.name, got, c.want)
		}
	}
}
