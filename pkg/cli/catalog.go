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

package cli

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var catalogYAML []byte

type Catalog struct {
	Version string           `yaml:"version"`
	Models  map[string]Model `yaml:"models"`
}

type Model struct {
	Name         string       `yaml:"name"`
	Description  string       `yaml:"description"`
	Size         string       `yaml:"size"`
	Quantization string       `yaml:"quantization"`
	Source       string       `yaml:"source"`
	ContextSize  int          `yaml:"context_size"`
	GPULayers    int32        `yaml:"gpu_layers"`
	UseCases     []string     `yaml:"use_cases"`
	Resources    ResourceSpec `yaml:"resources"`
	VRAMEstimate string       `yaml:"vram_estimate"`
	Tags         []string     `yaml:"tags"`
	Homepage     string       `yaml:"homepage"`
	Notes        string       `yaml:"notes,omitempty"`
}

type ResourceSpec struct {
	CPU       string `yaml:"cpu"`
	Memory    string `yaml:"memory"`
	GPUMemory string `yaml:"gpu_memory"`
}

var catalogInstance *Catalog

func LoadCatalog() (*Catalog, error) {
	if catalogInstance != nil {
		return catalogInstance, nil
	}

	var catalog Catalog
	if err := yaml.Unmarshal(catalogYAML, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse catalog: %w", err)
	}

	catalogInstance = &catalog
	return catalogInstance, nil
}

func GetModel(modelID string) (*Model, error) {
	catalog, err := LoadCatalog()
	if err != nil {
		return nil, err
	}

	model, exists := catalog.Models[modelID]
	if !exists {
		return nil, fmt.Errorf("model '%s' not found in catalog", modelID)
	}

	return &model, nil
}

func NewCatalogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Manage and browse the model catalog",
		Long: `Browse pre-configured LLM models in the catalog.

The catalog contains battle-tested models with optimized settings for
various use cases. Deploy any model with a single command.

Examples:
  # List all available models
  llmkube catalog list

  # Show detailed info about a model
  llmkube catalog info llama-3.1-8b

  # Filter by tags
  llmkube catalog list --tag code
`,
	}

	cmd.AddCommand(NewCatalogListCommand())
	cmd.AddCommand(NewCatalogInfoCommand())

	return cmd
}

func NewCatalogListCommand() *cobra.Command {
	var tagFilter string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all models in the catalog",
		Long: `List all pre-configured models in the catalog with their key specifications.

Examples:
  # List all models
  llmkube catalog list

  # Filter by tag
  llmkube catalog list --tag code
  llmkube catalog list --tag recommended
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCatalogList(tagFilter)
		},
	}

	cmd.Flags().StringVar(&tagFilter, "tag", "", "Filter models by tag (e.g., code, small, recommended)")

	return cmd
}

func NewCatalogInfoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info MODEL_ID",
		Short: "Show detailed information about a catalog model",
		Long: `Display detailed information about a specific model in the catalog.

Examples:
  llmkube catalog info llama-3.1-8b
  llmkube catalog info qwen-2.5-coder-7b
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCatalogInfo(args[0])
		},
	}

	return cmd
}

func runCatalogList(tagFilter string) error {
	catalog, err := LoadCatalog()
	if err != nil {
		return err
	}

	modelIDs := make([]string, 0, len(catalog.Models))
	for id := range catalog.Models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)

	filteredIDs := []string{}
	if tagFilter != "" {
		for _, id := range modelIDs {
			model := catalog.Models[id]
			if containsTag(model.Tags, tagFilter) {
				filteredIDs = append(filteredIDs, id)
			}
		}
		if len(filteredIDs) == 0 {
			fmt.Printf("No models found with tag '%s'\n", tagFilter)
			return nil
		}
		modelIDs = filteredIDs
	}

	fmt.Printf("\n📚 LLMKube Model Catalog (v%s)\n", catalog.Version)
	if tagFilter != "" {
		fmt.Printf("Filter: tag=%s\n", tagFilter)
	}
	fmt.Printf("═══════════════════════════════════════════════════════════════════════\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSIZE\tQUANT\tUSE CASE\tVRAM")
	_, _ = fmt.Fprintln(w, "──\t────\t────\t─────\t────────\t────")

	for _, id := range modelIDs {
		model := catalog.Models[id]
		useCase := "General"
		if len(model.UseCases) > 0 {
			useCase = formatUseCase(model.UseCases[0])
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			id,
			truncate(model.Name, 30),
			model.Size,
			model.Quantization,
			truncate(useCase, 20),
			model.VRAMEstimate,
		)
	}

	_ = w.Flush()

	fmt.Printf("\n💡 To deploy: llmkube deploy <MODEL_ID> --gpu\n")
	fmt.Printf("💡 For details: llmkube catalog info <MODEL_ID>\n\n")

	return nil
}

func runCatalogInfo(modelID string) error {
	model, err := GetModel(modelID)
	if err != nil {
		return err
	}

	fmt.Printf("\n📦 %s\n", model.Name)
	fmt.Printf("═══════════════════════════════════════════════════════════════════════\n\n")
	fmt.Printf("ID:              %s\n", modelID)
	fmt.Printf("Size:            %s parameters\n", model.Size)
	fmt.Printf("Quantization:    %s\n", model.Quantization)
	fmt.Printf("Context Size:    %s tokens\n", formatNumber(model.ContextSize))
	fmt.Printf("VRAM Estimate:   %s\n", model.VRAMEstimate)
	fmt.Printf("\n")

	fmt.Printf("Description:\n")
	fmt.Printf("  %s\n\n", model.Description)

	if len(model.UseCases) > 0 {
		fmt.Printf("Use Cases:\n")
		for _, uc := range model.UseCases {
			fmt.Printf("  • %s\n", formatUseCase(uc))
		}
		fmt.Printf("\n")
	}

	fmt.Printf("Resource Requirements:\n")
	fmt.Printf("  CPU:         %s\n", model.Resources.CPU)
	fmt.Printf("  Memory:      %s\n", model.Resources.Memory)
	fmt.Printf("  GPU Memory:  %s\n", model.Resources.GPUMemory)
	fmt.Printf("  GPU Layers:  %d\n", model.GPULayers)
	fmt.Printf("\n")

	if len(model.Tags) > 0 {
		fmt.Printf("Tags: %s\n\n", strings.Join(model.Tags, ", "))
	}

	if model.Notes != "" {
		fmt.Printf("⚠️  Notes: %s\n\n", model.Notes)
	}

	fmt.Printf("═══════════════════════════════════════════════════════════════════════\n")
	fmt.Printf("🚀 Quick Deploy:\n\n")
	fmt.Printf("  # CPU deployment\n")
	fmt.Printf("  llmkube deploy %s\n\n", modelID)
	fmt.Printf("  # GPU deployment (recommended)\n")
	fmt.Printf("  llmkube deploy %s --gpu\n\n", modelID)
	fmt.Printf("  # Custom configuration\n")
	fmt.Printf("  llmkube deploy %s --gpu --replicas 3 --context 16384\n\n", modelID)

	fmt.Printf("🔗 Homepage: %s\n", model.Homepage)
	fmt.Printf("🔗 Source:   %s\n\n", model.Source)

	return nil
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

func formatUseCase(uc string) string {
	words := strings.Split(uc, "-")
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d,%03d,%03d", n/1000000, (n/1000)%1000, n%1000)
}
