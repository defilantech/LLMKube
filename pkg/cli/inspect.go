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
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/defilantech/llmkube/pkg/gguf"
)

// NewInspectCommand creates the inspect command for local GGUF file inspection.
func NewInspectCommand() *cobra.Command {
	var showMetadata bool
	var showTensors bool

	cmd := &cobra.Command{
		Use:   "inspect <file.gguf>",
		Short: "Inspect a local GGUF model file",
		Long: `Parse and display metadata from a local GGUF model file.

Shows model architecture, quantization, context length, and other details
extracted directly from the GGUF file header.

Examples:
  # Show basic model info
  llmkube inspect model.gguf

  # Show all metadata key-value pairs
  llmkube inspect model.gguf --metadata

  # Show all tensor names and shapes
  llmkube inspect model.gguf --tensors
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(args[0], showMetadata, showTensors)
		},
	}

	cmd.Flags().BoolVar(&showMetadata, "metadata", false, "Show all metadata key-value pairs")
	cmd.Flags().BoolVar(&showTensors, "tensors", false, "Show all tensor names, shapes, and types")

	return cmd
}

func runInspect(path string, showMetadata, showTensors bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	parsed, err := gguf.Parse(bufio.NewReader(f))
	if err != nil {
		return fmt.Errorf("failed to parse GGUF: %w", err)
	}

	// Basic info
	fmt.Printf("Format:         GGUF v%d\n", parsed.Header.Version)
	if name := parsed.Name(); name != "" {
		fmt.Printf("Name:           %s\n", name)
	}
	if arch := parsed.Architecture(); arch != "" {
		fmt.Printf("Architecture:   %s\n", arch)
	}
	if quant := parsed.Quantization(); quant != "" {
		fmt.Printf("Quantization:   %s\n", quant)
	}
	if cl := parsed.ContextLength(); cl > 0 {
		fmt.Printf("Context Length: %d\n", cl)
	}
	if el := parsed.EmbeddingLength(); el > 0 {
		fmt.Printf("Embedding Dim:  %d\n", el)
	}
	if bc := parsed.BlockCount(); bc > 0 {
		fmt.Printf("Layers:         %d\n", bc)
	}
	if hc := parsed.HeadCount(); hc > 0 {
		fmt.Printf("Attn Heads:     %d\n", hc)
	}
	fmt.Printf("Tensors:        %d\n", parsed.Header.TensorCount)
	fmt.Printf("Metadata Keys:  %d\n", parsed.Header.MetadataKVCount)

	if showMetadata {
		fmt.Printf("\nMETADATA:\n")
		for _, kv := range parsed.Metadata {
			val := kv.Value.String()
			if len(val) > 80 {
				val = val[:77] + "..."
			}
			fmt.Printf("  %-40s %s\n", kv.Key, val)
		}
	}

	if showTensors {
		fmt.Printf("\nTENSORS:\n")
		for _, ti := range parsed.TensorInfo {
			dims := make([]string, len(ti.Dimensions))
			for i, d := range ti.Dimensions {
				dims[i] = fmt.Sprintf("%d", d)
			}
			fmt.Printf("  %-50s [%s] %s\n", ti.Name, strings.Join(dims, " x "), ti.Type)
		}
	}

	return nil
}
