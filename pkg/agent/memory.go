/*
Copyright 2026.

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
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	// MemoryCheckModeEnforce rejects an InferenceService when the pre-flight
	// memory check cannot be completed (fail closed). This is the default.
	MemoryCheckModeEnforce = "enforce"
	// MemoryCheckModeWarn preserves the legacy behavior of logging a warning
	// and starting the process anyway when the check cannot be completed.
	// An incomplete check allowed unsized models through on 2026-06-09 and
	// wired all 128 GB of host RAM, panicking the machine; use only as an
	// operational escape hatch.
	MemoryCheckModeWarn = "warn"
)

// MemoryPressureLevel represents the severity of memory pressure.
type MemoryPressureLevel int

const (
	MemoryPressureNormal   MemoryPressureLevel = 0
	MemoryPressureWarning  MemoryPressureLevel = 1
	MemoryPressureCritical MemoryPressureLevel = 2
)

func (l MemoryPressureLevel) String() string {
	switch l {
	case MemoryPressureWarning:
		return "warning"
	case MemoryPressureCritical:
		return "critical"
	default:
		return "normal"
	}
}

// MemoryProvider abstracts system memory queries for testability.
type MemoryProvider interface {
	TotalMemory() (uint64, error)
	AvailableMemory() (uint64, error)
	WiredMemory() (uint64, error)
	ProcessRSS(pid int) (uint64, error)
}

// MemoryEstimate holds the estimated memory requirements for a model.
type MemoryEstimate struct {
	WeightsBytes  uint64
	KVCacheBytes  uint64
	OverheadBytes uint64
	TotalBytes    uint64

	// RecurrentStateBytes is EstimateOptions.RecurrentStateBytes when the
	// KV term was computed, and is included in TotalBytes.
	RecurrentStateBytes uint64

	// KVCacheNotCounted, when set, says why TotalBytes holds no KV-cache
	// term although the service asks for more than the default context:
	// the attention shape was in neither the Model status nor a GGUF
	// header the agent could read, so the estimate is the file-size
	// heuristic. Admission messages carry it.
	KVCacheNotCounted string
}

// defaultContextSize is the --ctx-size the agent passes when the
// InferenceService sets no contextSize. Its KV cache is small enough for
// the file-size heuristic's 20% margin, so a missing attention shape is
// only reported above it.
const defaultContextSize = 2048

// MemoryBudget is the result of a memory budget check.
type MemoryBudget struct {
	Fits          bool
	TotalBytes    uint64
	BudgetBytes   uint64
	EstimateBytes uint64
	HeadroomBytes uint64
}

const overheadBytes = 512 * 1024 * 1024 // 512 MiB constant overhead

// EstimateOptions tweaks EstimateModelMemoryWithOptions's KV-cache calculation.
// Empty values reproduce the historical defaults (f16 K and V) and the
// historical embedding-width KV formula.
type EstimateOptions struct {
	// CacheTypeK is the llama.cpp --cache-type-k value (e.g. "q8_0", "turbo3").
	// Empty means f16. Unknown values fall back to f16 so the estimate
	// over-allocates rather than under, which is the safer default for a
	// pre-flight check.
	CacheTypeK string

	// CacheTypeV mirrors CacheTypeK for --cache-type-v.
	CacheTypeV string

	// HeadCount is the number of attention heads from the GGUF metadata.
	// 0 means unknown.
	HeadCount uint64

	// HeadCountKV is the number of key/value attention heads (grouped-query
	// attention). 0 means unknown: the GGUF did not provide a usable scalar
	// value, and the historical embedding-width formula is used instead.
	HeadCountKV uint64

	// KeyLength is the per-head key dimension. 0 means the GGUF did not
	// provide a usable scalar value; a missing side is then derived as
	// embeddingSize / HeadCount, which is llama.cpp's own default for an
	// absent length. The embedding-width formula applies when the GGUF has
	// no scalar head count, no KV head count, more KV heads than attention
	// heads, or a missing length while HeadCount does not divide
	// embeddingSize; with both lengths explicit a non-dividing HeadCount
	// still charges the head dims.
	KeyLength uint64

	// ValueLength is the per-head value dimension. 0 means the GGUF did not
	// provide a usable scalar value; resolved like KeyLength.
	ValueLength uint64

	// KVHeadLayers, when non-zero, is the KV head count summed over the
	// layers that hold a KV cache, read from a GGUF header's layer layout
	// (kvLayoutFromParsed). It replaces layerCount * HeadCountKV for files
	// where the two differ: a hybrid model's recurrent layers and a layer
	// whose per-layer head_count_kv is 0 hold no KV cache. HeadCount and
	// HeadCountKV are then needed only to derive a missing head dim.
	KVHeadLayers uint64

	// RecurrentStateBytes is the state a hybrid model's recurrent layers
	// hold instead of a KV cache, for every sequence llama-server sizes it
	// for. It is charged next to the KV term and does not grow with the
	// context.
	RecurrentStateBytes uint64
}

// cacheTypeBytesPerElement returns the per-element byte cost of a llama.cpp
// KV cache type. Quantized types include their per-block fp16 scale (and
// fp16 min for q*_1) overhead, so this approximates real disk/memory cost
// rather than the bare bit width. TurboQuant turbo3/turbo4 values come from
// TheTom's spec (https://github.com/ggml-org/llama.cpp/discussions/20969).
// The case labels are exactly the strings llama.cpp recognizes for
// --cache-type-k / --cache-type-v; promoting them to named constants would
// not improve safety since any typo would just move into the constant decl.
//
//nolint:goconst
func cacheTypeBytesPerElement(t string) float64 {
	switch t {
	case "", "f16":
		return 2.0
	case "f32":
		return 4.0
	case "q8_0":
		// 32 weights * 8 bits + fp16 scale = 272 bits / 32 = 8.5 bits = 1.0625 bytes
		return 1.0625
	case "q5_0":
		// 32 * 5 + 16 = 176 / 32 = 5.5 bits
		return 0.6875
	case "q5_1":
		// 32 * 5 + 32 (scale + min) = 192 / 32 = 6.0 bits
		return 0.75
	case "q4_0", "iq4_nl":
		// 32 * 4 + 16 = 144 / 32 = 4.5 bits
		return 0.5625
	case "q4_1":
		// 32 * 4 + 32 = 160 / 32 = 5.0 bits
		return 0.625
	case "turbo3":
		// 3.25 bits/element per TurboQuant spec
		return 0.40625
	case "turbo4":
		// 4.25 bits/element per TurboQuant spec
		return 0.53125
	default:
		// Unknown type: assume f16 to over-estimate; safer for pre-flight check.
		return 2.0
	}
}

// EstimateModelMemory estimates total memory needed to serve a model with
// llama.cpp's default f16 KV cache. Kept for backward compatibility with the
// historical signature; new call sites should prefer EstimateModelMemoryWithOptions
// so they can pass the actual configured cache types and get an accurate
// estimate when TurboQuant or any quantized cache is in use.
func EstimateModelMemory(fileSizeBytes uint64, layerCount, embeddingSize uint64, contextSize int) MemoryEstimate {
	return EstimateModelMemoryWithOptions(fileSizeBytes, layerCount, embeddingSize, contextSize, EstimateOptions{})
}

// resolveKVShape resolves the per-layer KV geometry for the GQA-aware
// estimate. ok is false whenever the GGUF did not provide a usable scalar
// attention shape, in which case the caller must use the historical
// embedding-width formula. A missing key_length or value_length is derived
// as embeddingSize/HeadCount because that is llama.cpp's own allocation
// default for an absent length: llama-model.cpp's generic loader sets
// n_embd_head_k_full and n_embd_head_v_full to n_embd/n_head independently
// and overrides each only when the GGUF carries its key, so the derived cell
// per side is what the server allocates rather than a guess. Real files
// split both ways: Llama 3.1 and Qwen 2.5 GGUFs carry head_count_kv but no
// lengths, while Qwen 3 and Gemma 3 carry all three; older deepseek2
// conversions carry all three with no *_mla keys, so llama-server runs the
// non-MLA path and the head-dim charge is exact. Current-converter deepseek2
// files are MLA layout (one KV head, lengths derived from kv_lora_rank):
// llama-server stores only the compressed key cache there, so the head-dim
// charge over-charges those files, which is the safe direction for a
// pre-flight check. Array-valued head_count_kv (Jamba, Nemotron-H) reads as
// 0 and takes the same fallback.
// Models that do fall back are over-charged by HeadCount/HeadCountKV, and
// the hybrid archs named above are charged further because their layer
// count includes the non-attention layers.
//
// With opts.KVHeadLayers set, the per-layer head counts already came from the
// file, so only the head dims are resolved here (a missing one still needs a
// scalar HeadCount that divides embeddingSize).
func resolveKVShape(embeddingSize uint64, opts EstimateOptions) (kvHeads, keyLen, valLen uint64, ok bool) {
	if opts.KVHeadLayers == 0 &&
		(opts.HeadCount == 0 || opts.HeadCountKV == 0 || opts.HeadCountKV > opts.HeadCount) {
		return 0, 0, 0, false
	}
	keyLen, valLen = opts.KeyLength, opts.ValueLength
	if keyLen == 0 || valLen == 0 {
		// HeadCount is non-zero here unless KVHeadLayers is set; a layout
		// with no scalar head count cannot derive a dim.
		if opts.HeadCount == 0 || embeddingSize%opts.HeadCount != 0 {
			return 0, 0, 0, false
		}
		derived := embeddingSize / opts.HeadCount
		if keyLen == 0 {
			keyLen = derived
		}
		if valLen == 0 {
			valLen = derived
		}
	}
	return opts.HeadCountKV, keyLen, valLen, true
}

// EstimateModelMemoryWithOptions is EstimateModelMemory parameterized on the
// configured KV cache types. The KV-cache term becomes
// layers * embedding * context * (bytesPerK + bytesPerV) where bytesPerK / V
// come from cacheTypeBytesPerElement. With f16/f16 (the empty-string default)
// it produces the same result as EstimateModelMemory. When the GGUF attention
// shape is known (HeadCountKV between 1 and HeadCount, with both head dims
// present explicitly or derived as embeddingSize/HeadCount, which is
// llama.cpp's own default for an absent length), the
// KV term uses the grouped-query-attention shape
// layers * headCountKV * context * (keyLength * bytesPerK + valueLength * bytesPerV)
// instead. The GQA term is smaller than the embedding-width formula when
// kvHeads*(keyLen+valLen) is below embeddingSize (Llama-class GQA), larger
// when it exceeds it (head-dim models like Gemma, where heads*head_dim
// outruns the embedding width, so a model admitted under the old estimate
// can be refused after this change), and identical for a model with no GQA
// metadata at all; all three directions are pinned by test.
// Regardless of shape, contextSize is the process-wide --ctx-size pool:
// llama-server shares it across --parallel slots (total KV cells track the
// context size, not the context size times the slot count), so the estimate
// takes no slot factor.
func EstimateModelMemoryWithOptions(
	fileSizeBytes uint64,
	layerCount, embeddingSize uint64,
	contextSize int,
	opts EstimateOptions,
) MemoryEstimate {
	if layerCount == 0 || embeddingSize == 0 {
		return fallbackEstimate(fileSizeBytes)
	}

	// contextSize == 0 routes to the legacy branch, which yields a zero KV
	// term for it; a negative value wraps in the legacy uint64 conversion
	// and lands in the overflow fallback. Guarding here keeps the divisions
	// below well-defined for any caller.
	if kvHeads, keyLen, valLen, ok := resolveKVShape(embeddingSize, opts); ok && contextSize > 0 {
		ctx := uint64(contextSize) //nolint:gosec // G115: contextSize is CRD-validated ≥128 upstream
		headLayers := opts.KVHeadLayers
		if headLayers == 0 {
			headLayers = layerCount * kvHeads
			if headLayers/layerCount != kvHeads {
				return fallbackEstimate(fileSizeBytes)
			}
		}
		base := headLayers * ctx
		if base/ctx != headLayers {
			return fallbackEstimate(fileSizeBytes)
		}
		kElems := base * keyLen
		if kElems/base != keyLen {
			return fallbackEstimate(fileSizeBytes)
		}
		vElems := base * valLen
		if vElems/base != valLen {
			return fallbackEstimate(fileSizeBytes)
		}

		bytesK := cacheTypeBytesPerElement(opts.CacheTypeK)
		bytesV := cacheTypeBytesPerElement(opts.CacheTypeV)
		// Bound each side's byte total to MaxUint64/4 so the float64 sum
		// stays below 2^63 and the uint64 conversion is defined; the divisor
		// is the per-element cost rounded up, not a fixed 8. (f16 keeps the
		// old 8; q5_0 loosens to 4 and f32 tightens to 16.) The divisor is
		// never 0: cacheTypeBytesPerElement returns at least 0.40625 for
		// every case it defines. Above 2^53 elements the float64 products
		// round, so the converted total is approximate there; the guard
		// keeps it in range regardless.
		if kElems > math.MaxUint64/(4*uint64(math.Ceil(bytesK))) ||
			vElems > math.MaxUint64/(4*uint64(math.Ceil(bytesV))) {
			return fallbackEstimate(fileSizeBytes)
		}
		kvCache := uint64(math.Ceil(float64(kElems)*bytesK + float64(vElems)*bytesV))

		// kvCache is at most MaxUint64/2 (each side is bounded to /4 above).
		// Bounding the recurrent state to /4 keeps every term but the weights
		// below MaxUint64 together, so the single check below catches a wrap.
		if opts.RecurrentStateBytes > math.MaxUint64/4 {
			return fallbackEstimate(fileSizeBytes)
		}
		total := fileSizeBytes + kvCache + opts.RecurrentStateBytes + uint64(overheadBytes)
		if total < fileSizeBytes { // addition overflow
			return fallbackEstimate(fileSizeBytes)
		}
		return MemoryEstimate{
			WeightsBytes:        fileSizeBytes,
			KVCacheBytes:        kvCache,
			RecurrentStateBytes: opts.RecurrentStateBytes,
			OverheadBytes:       uint64(overheadBytes),
			TotalBytes:          total,
		}
	}

	// Guard against overflow in the multiplication chain. The integer math
	// here mirrors the historical behavior; the fractional bytes-per-element
	// multiplication happens once at the end via float64.
	layerEmbed := layerCount * embeddingSize
	if layerEmbed/layerCount != embeddingSize {
		return fallbackEstimate(fileSizeBytes)
	}
	ctx := uint64(contextSize) //nolint:gosec // G115: contextSize is CRD-validated ≥128 upstream
	product := layerEmbed * ctx
	if ctx != 0 && product/ctx != layerEmbed {
		return fallbackEstimate(fileSizeBytes)
	}

	bytesPerKV := cacheTypeBytesPerElement(opts.CacheTypeK) + cacheTypeBytesPerElement(opts.CacheTypeV)
	// Bound the byte total the same way the GQA branch bounds its sides:
	// product fits uint64, but product times the per-element cost (8.0 at
	// f32/f32) can leave uint64 range and convert to an
	// implementation-defined value. Divisor is the cost rounded up, times
	// four for headroom on both sides of the ceil.
	if product > math.MaxUint64/(4*uint64(math.Ceil(bytesPerKV))) {
		return fallbackEstimate(fileSizeBytes)
	}
	kvCache := uint64(math.Ceil(float64(product) * bytesPerKV))

	total := fileSizeBytes + kvCache + uint64(overheadBytes)
	if total < fileSizeBytes { // addition overflow
		return fallbackEstimate(fileSizeBytes)
	}
	return MemoryEstimate{
		WeightsBytes:  fileSizeBytes,
		KVCacheBytes:  kvCache,
		OverheadBytes: uint64(overheadBytes),
		TotalBytes:    total,
	}
}

// fallbackEstimate uses a heuristic (fileSize * 1.2 + overhead) when GGUF
// metadata is unavailable or the KV cache calculation would overflow.
func fallbackEstimate(fileSizeBytes uint64) MemoryEstimate {
	scaled := uint64(math.Ceil(float64(fileSizeBytes) * 1.2))
	total := scaled + uint64(overheadBytes)
	return MemoryEstimate{
		WeightsBytes:  fileSizeBytes,
		KVCacheBytes:  0,
		OverheadBytes: scaled - fileSizeBytes + uint64(overheadBytes),
		TotalBytes:    total,
	}
}

// DefaultMemoryFraction returns the fraction of total system memory to use
// as a budget for model serving. Returns 0.67 for systems ≤36GB, 0.75 for larger.
func DefaultMemoryFraction(totalBytes uint64) float64 {
	const threshold = 36 * 1024 * 1024 * 1024 // 36 GiB
	if totalBytes <= threshold {
		return 0.67
	}
	return 0.75
}

// CheckMemoryBudget checks whether a model's estimated memory fits within
// the system's memory budget (total * fraction).
func CheckMemoryBudget(provider MemoryProvider, estimate MemoryEstimate, fraction float64) (*MemoryBudget, error) {
	total, err := provider.TotalMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get total memory: %w", err)
	}

	budget := uint64(float64(total) * fraction)
	fits := estimate.TotalBytes <= budget

	var headroom uint64
	if fits {
		headroom = budget - estimate.TotalBytes
	}

	return &MemoryBudget{
		Fits:          fits,
		TotalBytes:    total,
		BudgetBytes:   budget,
		EstimateBytes: estimate.TotalBytes,
		HeadroomBytes: headroom,
	}, nil
}

// formatMemory formats a byte count into a human-readable string like "24.3 GB" or "512 MB".
func formatMemory(bytes uint64) string {
	const (
		gb = 1024 * 1024 * 1024
		mb = 1024 * 1024
	)
	if bytes >= gb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/float64(mb))
}

// remoteModelSize probes an http(s) model source with a HEAD request and
// returns its Content-Length. It is the last-resort size source for the
// pre-flight memory check: used when the model is not on disk yet and the
// Model CR carries no usable status size, so admission can still be decided
// before the download begins instead of failing open. httpClient must be the
// SSRF-guarded client (safehttp.NewClient): the source is user-supplied.
func remoteModelSize(ctx context.Context, httpClient *http.Client, source string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, source, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, &sizeProbeStatusError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("no Content-Length in HEAD response")
	}
	return uint64(resp.ContentLength), nil
}

// sizeProbeStatusError is a size probe HEAD answered with a status other than
// 200. It carries the code so transientSizeProbeError can tell an origin that
// is down for the moment (5xx, 429) from one that answered definitively (403,
// 404).
type sizeProbeStatusError struct {
	StatusCode int
	Status     string
}

func (e *sizeProbeStatusError) Error() string {
	return "bad status: " + e.Status
}

// s3ObjectSize returns an s3:// object's byte size from a SigV4-signed HEAD
// through the same SSRF-guarded client and sourceSecretRef credentials the S3
// download uses (pkg/agent/s3.go); it never opens a second, unsigned path.
func s3ObjectSize(ctx context.Context, exec *MetalExecutor, source string, key sourceSecretKey) (uint64, error) {
	creds, err := exec.resolveS3Credentials(ctx, source, key)
	if err != nil {
		return 0, err
	}
	client, objectURL, err := exec.s3DownloadClient(source, creds)
	if err != nil {
		return 0, err
	}
	defer client.CloseIdleConnections()
	return remoteModelSize(ctx, client, objectURL)
}

// s3SizeProbe sizes an s3:// Model source for the pre-flight memory check.
// It reuses the registered llama.cpp executor so the probe dials with the
// same allowlist, resolver and CA trust as the download; a unit test that has
// not called buildExecutors gets an equivalently wired stand-in.
func (a *MetalAgent) s3SizeProbe(ctx context.Context, model *inferencev1alpha1.Model) (uint64, error) {
	exec, _ := a.executors[runtimeLlamaServer].(*MetalExecutor)
	if exec == nil {
		exec = NewMetalExecutor(a.config.LlamaServerBin, a.config.ModelStorePath, a.logger,
			WithKubeClient(a.config.K8sClient, nil),
			WithAllowUnlabeledSourceSecrets(a.config.AllowUnlabeledSourceSecrets),
			WithAllowedDownloadHosts(a.config.AllowedDownloadHosts))
	}
	return s3ObjectSize(ctx, exec, model.Spec.Source, sourceSecretKeyFor(model.Namespace, model.Spec.SourceSecretRef))
}

// parseSize parses a human-readable size string (as produced by model_controller's
// formatBytes, e.g. "4.5 GiB", "512.0 MiB", "1024 B") back to bytes.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid size format %q: expected \"<number> <unit>\"", s)
	}

	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size number %q: %w", parts[0], err)
	}

	var multiplier float64
	switch strings.ToUpper(parts[1]) {
	case "B":
		multiplier = 1
	case "KIB":
		multiplier = 1024
	case "MIB":
		multiplier = 1024 * 1024
	case "GIB":
		multiplier = 1024 * 1024 * 1024
	case "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown size unit %q", parts[1])
	}

	return uint64(value * multiplier), nil
}

const (
	// BudgetModeAbsolute indicates a fixed byte budget from the CRD.
	BudgetModeAbsolute = "absolute"
	// BudgetModeFraction indicates a fraction-of-memory budget.
	BudgetModeFraction = "fraction"
)

// ResolvedBudget describes the effective memory budget after applying the
// precedence chain: CRD absolute → CRD fraction → agent flag → auto-detect.
type ResolvedBudget struct {
	// Mode is BudgetModeAbsolute when a fixed byte budget is in effect, or
	// BudgetModeFraction when the budget is a percentage of total system memory.
	Mode string
	// Bytes is the absolute budget in bytes. Set only when Mode == "absolute".
	Bytes uint64
	// Fraction is the fraction of total memory. Set only when Mode == "fraction".
	Fraction float64
	// Source describes where the value came from for logging.
	Source string
}

// ResolveMemoryBudget implements the precedence chain:
//  1. model.Spec.Hardware.MemoryBudget  (absolute byte limit)
//  2. model.Spec.Hardware.MemoryFraction (fraction of total RAM)
//  3. agentFraction                      (--memory-fraction flag)
//
// Returns an error only if MemoryBudget is set but cannot be parsed.
func ResolveMemoryBudget(hardware *inferencev1alpha1.HardwareSpec, agentFraction float64) (ResolvedBudget, error) {
	if hardware != nil && hardware.MemoryBudget != "" {
		qty, err := resource.ParseQuantity(hardware.MemoryBudget)
		if err != nil {
			return ResolvedBudget{}, fmt.Errorf("invalid memoryBudget %q: %w", hardware.MemoryBudget, err)
		}
		if qty.Value() <= 0 {
			return ResolvedBudget{}, fmt.Errorf("memoryBudget must be positive, got %q", hardware.MemoryBudget)
		}
		return ResolvedBudget{
			Mode:   BudgetModeAbsolute,
			Bytes:  uint64(qty.Value()), //nolint:gosec // G115: guarded positive by the qty.Value() <= 0 check above
			Source: "crd-budget",
		}, nil
	}

	if hardware != nil && hardware.MemoryFraction != nil {
		f := *hardware.MemoryFraction
		if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1.0 {
			return ResolvedBudget{}, fmt.Errorf("memoryFraction must be between 0.0 (exclusive) and 1.0 (inclusive), got %f", f)
		}
		return ResolvedBudget{
			Mode:     BudgetModeFraction,
			Fraction: f,
			Source:   "crd-fraction",
		}, nil
	}

	return ResolvedBudget{
		Mode:     BudgetModeFraction,
		Fraction: agentFraction,
		Source:   "agent-flag",
	}, nil
}

// CheckMemoryBudgetAbsolute checks whether a model's estimated memory fits
// within a fixed byte budget (as opposed to a fraction of system memory).
func CheckMemoryBudgetAbsolute(budgetBytes uint64, estimate MemoryEstimate) *MemoryBudget {
	fits := estimate.TotalBytes <= budgetBytes

	var headroom uint64
	if fits {
		headroom = budgetBytes - estimate.TotalBytes
	}

	return &MemoryBudget{
		Fits:          fits,
		TotalBytes:    0, // not applicable for absolute budgets
		BudgetBytes:   budgetBytes,
		EstimateBytes: estimate.TotalBytes,
		HeadroomBytes: headroom,
	}
}
