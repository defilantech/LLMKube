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

	// ChartResourceNames holds plain metadata.name values scraped from Helm
	// chart templates (non-templated literals only). Used by the advisory
	// grounding-breadth check to validate resource names in docs.
	ChartResourceNames map[string]bool

	// ExporterMetricPrefixes is the set of metric-name prefixes from external
	// Prometheus exporters that are deployed alongside LLMKube (e.g. "DCGM_FI_",
	// "node_"). When non-empty, the advisory grounding-breadth check flags any
	// token that looks like a metric name but does not start with a known prefix.
	ExporterMetricPrefixes []string
}

var (
	metricNameRe = regexp.MustCompile(`"(llmkube_[a-z0-9_]+)"`)
	cobraUseRe   = regexp.MustCompile(`Use:\s*"([a-zA-Z][a-zA-Z0-9_-]*)`)

	// chartMetaNameRe matches a plain (non-templated) metadata.name value in a
	// Helm chart YAML template. Values containing "{{" are skipped (they are
	// Go-template expressions, not literal names).
	chartMetaNameRe = regexp.MustCompile(`^\s*name:\s*(\S+)\s*$`)
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

// scanChartResources walks chartsDir recursively for *.yaml and *.yml files
// and captures every plain (non-templated) metadata.name value. Names that
// contain "{{" (Helm template syntax) are skipped.
func scanChartResources(chartsDir string, gt *GroundTruth) {
	_ = filepath.WalkDir(chartsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipScanDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return nil
		}
		b, _ := os.ReadFile(p)
		inMetadata := false
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "metadata:" {
				inMetadata = true
				continue
			}
			// A new top-level or same-level key resets the metadata context.
			// We use a simple heuristic: if the line is not indented and is a
			// key (contains ':'), leave metadata scope.
			if inMetadata {
				if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && strings.Contains(line, ":") {
					inMetadata = false
				}
				if m := chartMetaNameRe.FindStringSubmatch(line); m != nil {
					val := m[1]
					if !strings.Contains(val, "{{") {
						gt.ChartResourceNames[val] = true
					}
					inMetadata = false // name seen; no need to stay in metadata block
				}
			}
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
		ChartResourceNames: map[string]bool{},
		ExporterMetricPrefixes: []string{
			"DCGM_FI_", "node_", "container_", "kube_", "go_", "process_",
		},
	}
	if err := loadCRDBases(crdBasesDir, gt); err != nil {
		return nil, err
	}
	if metricsDir != "" {
		scanMetrics(metricsDir, gt)
		scanChartResources(filepath.Join(metricsDir, "charts"), gt)
	}
	if cmdDir != "" {
		scanCLICommands(cmdDir, gt)
	}
	return gt, nil
}

// loadCRDBases reads *.yaml files from crdBasesDir and populates gt with CRD
// groups, kinds, and spec field names. An empty or missing dir string is a no-op.
func loadCRDBases(crdBasesDir string, gt *GroundTruth) error {
	if crdBasesDir == "" {
		return nil
	}
	entries, err := os.ReadDir(crdBasesDir)
	if err != nil {
		// Missing dir is not an error: the advisory check runs without CRD
		// context when the workspace lacks a config/crd/bases tree (e.g. in
		// tests or on first-time clones).
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(crdBasesDir, e.Name()))
		if err != nil {
			return err
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
	return nil
}
