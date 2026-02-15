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
	"sort"
	"time"
)

func calculateSummary(
	opts *benchmarkOptions, endpoint string, results []BenchmarkResult, startTime time.Time,
) BenchmarkSummary {
	summary := BenchmarkSummary{
		ServiceName:  opts.name,
		Namespace:    opts.namespace,
		Endpoint:     endpoint,
		Iterations:   opts.iterations,
		PromptTokens: 0,
		MaxTokens:    opts.maxTokens,
		Results:      results,
		Timestamp:    startTime,
		Duration:     time.Since(startTime),
	}

	latencies := make([]float64, 0, len(results))
	genToks := make([]float64, 0, len(results))
	promptToks := make([]float64, 0, len(results))

	for _, r := range results {
		if r.Error != "" {
			summary.FailedRuns++
			continue
		}
		summary.SuccessfulRuns++
		summary.PromptTokens = r.PromptTokens // They should all be the same

		latencies = append(latencies, r.TotalTimeMs)
		if r.GenerationToksPerSec > 0 {
			genToks = append(genToks, r.GenerationToksPerSec)
		}
		if r.PromptToksPerSec > 0 {
			promptToks = append(promptToks, r.PromptToksPerSec)
		}
	}

	if len(latencies) == 0 {
		return summary
	}

	sort.Float64s(latencies)
	sort.Float64s(genToks)

	summary.LatencyMin = latencies[0]
	summary.LatencyMax = latencies[len(latencies)-1]
	summary.LatencyMean = mean(latencies)
	summary.LatencyP50 = percentile(latencies, 50)
	summary.LatencyP95 = percentile(latencies, 95)
	summary.LatencyP99 = percentile(latencies, 99)

	if len(genToks) > 0 {
		summary.GenerationToksPerSecMean = mean(genToks)
		summary.GenerationToksPerSecMin = genToks[0]
		summary.GenerationToksPerSecMax = genToks[len(genToks)-1]
	}
	if len(promptToks) > 0 {
		summary.PromptToksPerSecMean = mean(promptToks)
	}

	return summary
}

func calculateStressSummary(
	opts *benchmarkOptions, endpoint string, results []BenchmarkResult, startTime time.Time, concurrency int,
) StressTestSummary {
	baseSummary := calculateSummary(opts, endpoint, results, startTime)

	summary := StressTestSummary{
		BenchmarkSummary: baseSummary,
		Concurrency:      concurrency,
		TargetDuration:   opts.duration,
		TotalRequests:    int64(len(results)),
	}

	elapsed := time.Since(startTime).Seconds()
	if elapsed > 0 {
		summary.RequestsPerSec = float64(len(results)) / elapsed
	}

	if len(results) > 0 {
		summary.ErrorRate = float64(baseSummary.FailedRuns) / float64(len(results)) * 100
	}

	genToks := make([]float64, 0, len(results))
	for _, r := range results {
		if r.Error == "" && r.GenerationToksPerSec > 0 {
			genToks = append(genToks, r.GenerationToksPerSec)
		}
	}

	if len(genToks) > 0 {
		sort.Float64s(genToks)
		summary.PeakToksPerSec = genToks[len(genToks)-1]

		mean := summary.GenerationToksPerSecMean
		var sumSquares float64
		for _, v := range genToks {
			diff := v - mean
			sumSquares += diff * diff
		}
		summary.ToksPerSecStdDev = sumSquares / float64(len(genToks))
		if summary.ToksPerSecStdDev > 0 {
			summary.ToksPerSecStdDev = float64(int(summary.ToksPerSecStdDev*100)) / 100 // sqrt approximation via variance
		}
	}

	return summary
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func percentile(sortedValues []float64, p float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	if len(sortedValues) == 1 {
		return sortedValues[0]
	}

	index := (p / 100.0) * float64(len(sortedValues)-1)
	lower := int(index)
	upper := lower + 1
	if upper >= len(sortedValues) {
		return sortedValues[len(sortedValues)-1]
	}

	weight := index - float64(lower)
	return sortedValues[lower]*(1-weight) + sortedValues[upper]*weight
}
