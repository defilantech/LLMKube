package grounding

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// GroundTruth is the set of LLMKube-owned symbols a reference may name.
type GroundTruth struct {
	Groups     map[string]bool            // API groups, e.g. "inference.llmkube.dev"
	Kinds      map[string]bool            // CRD kinds, e.g. "InferenceService"
	SpecFields map[string]map[string]bool // kind -> set of spec.<field> names
	Metrics    map[string]bool            // metric names, e.g. "llmkube_inferenceservice_phase"
	CLICmds    map[string]bool            // llmkube subcommands, e.g. "deploy"
}

var (
	metricNameRe = regexp.MustCompile(`"(llmkube_[a-z0-9_]+)"`)
	cobraUseRe   = regexp.MustCompile(`Use:\s*"([a-zA-Z][a-zA-Z0-9_-]*)`)
)

// skipScanDir skips version-control, vendored, build, and fixture directories
// so a repo-wide scan stays fast and does not ingest testdata or tool binaries.
func skipScanDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", "testdata", "bin":
		return true
	}
	return false
}

// scanMetrics walks dir for Go files and records llmkube_* metric-name string
// literals. Best-effort: an unreadable file is skipped.
func scanMetrics(dir string, gt *GroundTruth) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipScanDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range metricNameRe.FindAllStringSubmatch(string(b), -1) {
			gt.Metrics[m[1]] = true
		}
		return nil
	})
}

// scanCLICommands walks dir for cobra `Use: "<verb>"` declarations.
func scanCLICommands(dir string, gt *GroundTruth) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipScanDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range cobraUseRe.FindAllStringSubmatch(string(b), -1) {
			gt.CLICmds[m[1]] = true
		}
		return nil
	})
}

// crdDoc is the minimal CRD shape we parse.
type crdDoc struct {
	Spec struct {
		Group string `yaml:"group"`
		Names struct {
			Kind string `yaml:"kind"`
		} `yaml:"names"`
		Versions []struct {
			Schema struct {
				OpenAPIV3Schema struct {
					Properties struct {
						Spec struct {
							Properties map[string]yaml.Node `yaml:"properties"`
						} `yaml:"spec"`
					} `yaml:"properties"`
				} `yaml:"openAPIV3Schema"`
			} `yaml:"schema"`
		} `yaml:"versions"`
	} `yaml:"spec"`
}

// LoadGroundTruth scans crdBasesDir for CRD YAMLs and builds the symbol set.
// metricsDir and cmdDir are scanned in a later task; empty strings skip them.
func LoadGroundTruth(crdBasesDir, metricsDir, cmdDir string) (*GroundTruth, error) {
	gt := &GroundTruth{
		Groups: map[string]bool{}, Kinds: map[string]bool{},
		SpecFields: map[string]map[string]bool{},
		Metrics:    map[string]bool{}, CLICmds: map[string]bool{},
	}
	entries, err := os.ReadDir(crdBasesDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(crdBasesDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var doc crdDoc
		if err := yaml.Unmarshal(b, &doc); err != nil {
			continue // a non-CRD yaml; skip rather than fail
		}
		if doc.Spec.Group == "" || doc.Spec.Names.Kind == "" {
			continue
		}
		gt.Groups[doc.Spec.Group] = true
		gt.Kinds[doc.Spec.Names.Kind] = true
		fields := gt.SpecFields[doc.Spec.Names.Kind]
		if fields == nil {
			fields = map[string]bool{}
			gt.SpecFields[doc.Spec.Names.Kind] = fields
		}
		for _, v := range doc.Spec.Versions {
			for name := range v.Schema.OpenAPIV3Schema.Properties.Spec.Properties {
				fields[name] = true
			}
		}
	}
	if metricsDir != "" {
		scanMetrics(metricsDir, gt)
	}
	if cmdDir != "" {
		scanCLICommands(cmdDir, gt)
	}
	return gt, nil
}
