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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func getReportPath(opts *benchmarkOptions) (string, error) {
	if opts.report != "" {
		return opts.report, nil
	}
	if opts.reportDir != "" {
		if err := os.MkdirAll(opts.reportDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create report directory: %w", err)
		}
		filename := fmt.Sprintf("benchmark-%s.md", time.Now().Format("20060102-150405"))
		return filepath.Join(opts.reportDir, filename), nil
	}
	return "", nil
}

func newReportWriter(opts *benchmarkOptions) (*ReportWriter, error) {
	path, err := getReportPath(opts)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create report file: %w", err)
	}

	rw := &ReportWriter{
		file:      file,
		startTime: time.Now(),
		opts:      opts,
	}

	if err := rw.writeHeader(); err != nil {
		_ = file.Close()
		return nil, err
	}

	fmt.Printf("📄 Report: %s\n", path)
	return rw, nil
}

func (rw *ReportWriter) writeHeader() error {
	_, err := fmt.Fprintf(rw.file, "# LLMKube Benchmark Report\n\n")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "**Generated:** %s  \n", rw.startTime.Format("2006-01-02 15:04:05"))
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "**Host:** %s (%s/%s)  \n", getHostname(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	if rw.opts.gpu {
		accelerator := rw.opts.accelerator
		if accelerator == "" {
			accelerator = acceleratorCUDA
		}
		_, err = fmt.Fprintf(rw.file, "**Accelerator:** %s (GPU Count: %d)  \n", accelerator, rw.opts.gpuCount)
	} else {
		_, err = fmt.Fprintf(rw.file, "**Accelerator:** CPU  \n")
	}
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "\n---\n\n")
	return err
}

func (rw *ReportWriter) writeSection(title string, content string) error {
	_, err := fmt.Fprintf(rw.file, "## %s\n\n%s\n\n", title, content)
	return err
}

func (rw *ReportWriter) writeBenchmarkResult(summary *BenchmarkSummary) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Service:** %s  \n", summary.ServiceName))
	buf.WriteString(fmt.Sprintf("**Namespace:** %s  \n", summary.Namespace))
	buf.WriteString(fmt.Sprintf("**Duration:** %s  \n\n", summary.Duration.Round(time.Second)))

	buf.WriteString("| Metric | Value |\n")
	buf.WriteString("|--------|-------|\n")
	buf.WriteString(fmt.Sprintf("| Iterations | %d |\n", summary.Iterations))
	buf.WriteString(fmt.Sprintf("| Success Rate | %.1f%% |\n",
		float64(summary.SuccessfulRuns)/float64(summary.Iterations)*100))
	buf.WriteString(fmt.Sprintf("| Generation (tok/s) | %.1f |\n", summary.GenerationToksPerSecMean))
	buf.WriteString(fmt.Sprintf("| Latency P50 | %.0f ms |\n", summary.LatencyP50))
	buf.WriteString(fmt.Sprintf("| Latency P99 | %.0f ms |\n", summary.LatencyP99))

	return rw.writeSection("Benchmark Results", buf.String())
}

func (rw *ReportWriter) writeStressResult(summary *StressTestSummary) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Service:** %s  \n", summary.ServiceName))
	buf.WriteString(fmt.Sprintf("**Concurrency:** %d  \n", summary.Concurrency))
	if summary.TargetDuration > 0 {
		buf.WriteString(fmt.Sprintf("**Target Duration:** %s  \n", summary.TargetDuration))
	}
	buf.WriteString(fmt.Sprintf("**Actual Duration:** %s  \n\n", summary.Duration.Round(time.Second)))

	buf.WriteString("| Metric | Value |\n")
	buf.WriteString("|--------|-------|\n")
	buf.WriteString(fmt.Sprintf("| Total Requests | %d |\n", summary.TotalRequests))
	buf.WriteString(fmt.Sprintf("| Requests/sec | %.2f |\n", summary.RequestsPerSec))
	buf.WriteString(fmt.Sprintf("| Error Rate | %.1f%% |\n", summary.ErrorRate))
	buf.WriteString(fmt.Sprintf("| Generation (tok/s) | %.1f |\n", summary.GenerationToksPerSecMean))
	buf.WriteString(fmt.Sprintf("| Peak (tok/s) | %.1f |\n", summary.PeakToksPerSec))
	buf.WriteString(fmt.Sprintf("| Latency P50 | %.0f ms |\n", summary.LatencyP50))
	buf.WriteString(fmt.Sprintf("| Latency P99 | %.0f ms |\n", summary.LatencyP99))

	return rw.writeSection("Stress Test Results", buf.String())
}

func (rw *ReportWriter) writeSweepResults(sweepReport *SweepReport) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Sweep Type:** %s  \n", sweepReport.SweepType))
	buf.WriteString(fmt.Sprintf("**Values Tested:** %s  \n", strings.Join(sweepReport.Values, ", ")))
	buf.WriteString(fmt.Sprintf("**Duration:** %s  \n\n", sweepReport.Duration.Round(time.Second)))

	buf.WriteString("| Value | Gen tok/s | P50 (ms) | P99 (ms) | Requests | RPS | Error% | Status |\n")
	buf.WriteString("|-------|-----------|----------|----------|----------|-----|--------|--------|\n")

	for _, result := range sweepReport.Results {
		status := statusIconSuccess
		genToks := "-"
		p50 := "-"
		p99 := "-"
		requests := "-"
		rps := "-"
		errRate := "-"

		if result.Error != "" {
			status = statusIconFailed
		} else if result.Stress != nil {
			genToks = fmt.Sprintf("%.1f", result.Stress.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", result.Stress.LatencyP50)
			p99 = fmt.Sprintf("%.0f", result.Stress.LatencyP99)
			requests = fmt.Sprintf("%d", result.Stress.TotalRequests)
			rps = fmt.Sprintf("%.1f", result.Stress.RequestsPerSec)
			errRate = fmt.Sprintf("%.1f", result.Stress.ErrorRate)
		} else if result.Summary != nil {
			genToks = fmt.Sprintf("%.1f", result.Summary.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", result.Summary.LatencyP50)
			p99 = fmt.Sprintf("%.0f", result.Summary.LatencyP99)
			requests = fmt.Sprintf("%d", result.Summary.Iterations)
		}

		buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			result.Value, genToks, p50, p99, requests, rps, errRate, status))
	}

	return rw.writeSection(sweepReport.SweepType+" Sweep Results", buf.String())
}

func (rw *ReportWriter) writeGPUMetrics(metrics []GPUMetric) error {
	if len(metrics) == 0 {
		return nil
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("**Samples:** %d  \n\n", len(metrics)))

	var peakMem, peakUtil, peakTemp, peakPower int
	for _, m := range metrics {
		if m.MemoryUsedMB > peakMem {
			peakMem = m.MemoryUsedMB
		}
		if m.UtilPercent > peakUtil {
			peakUtil = m.UtilPercent
		}
		if m.TempCelsius > peakTemp {
			peakTemp = m.TempCelsius
		}
		if m.PowerWatts > peakPower {
			peakPower = m.PowerWatts
		}
	}

	buf.WriteString("| Metric | Peak Value |\n")
	buf.WriteString("|--------|------------|\n")
	if len(metrics) > 0 && metrics[0].MemoryTotalMB > 0 {
		buf.WriteString(fmt.Sprintf("| Memory | %d / %d MB (%.1f%%) |\n",
			peakMem, metrics[0].MemoryTotalMB, float64(peakMem)/float64(metrics[0].MemoryTotalMB)*100))
	}
	buf.WriteString(fmt.Sprintf("| Utilization | %d%% |\n", peakUtil))
	if peakTemp > 0 {
		buf.WriteString(fmt.Sprintf("| Temperature | %d°C |\n", peakTemp))
	}
	if peakPower > 0 {
		buf.WriteString(fmt.Sprintf("| Power | %d W |\n", peakPower))
	}

	return rw.writeSection("GPU Metrics", buf.String())
}

func (rw *ReportWriter) writeComparisonReport(report ComparisonReport) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Models:** %d  \n", len(report.Models)))
	if report.GPUEnabled {
		buf.WriteString(fmt.Sprintf("**Accelerator:** %s  \n", report.Accelerator))
		buf.WriteString(fmt.Sprintf("**GPU Count:** %d  \n", report.GPUCount))
	}
	if report.IsStressTest {
		buf.WriteString(fmt.Sprintf("**Concurrency:** %d  \n", report.Concurrency))
		if report.TargetDuration > 0 {
			buf.WriteString(fmt.Sprintf("**Duration:** %s per model  \n", report.TargetDuration))
		}
	}
	buf.WriteString(fmt.Sprintf("**Iterations:** %d per model  \n", report.Iterations))
	buf.WriteString(fmt.Sprintf("**Max Tokens:** %d  \n\n", report.MaxTokens))

	if report.IsStressTest {
		buf.WriteString("| Model | Size | Requests | RPS | tok/s | P50 | P99 | Error% | Status |\n")
		buf.WriteString("|-------|------|----------|-----|-------|-----|-----|--------|--------|\n")
	} else {
		buf.WriteString("| Model | Size | Gen tok/s | P50 (ms) | P99 (ms) | VRAM | Status |\n")
		buf.WriteString("|-------|------|-----------|----------|----------|------|--------|\n")
	}

	for _, m := range report.Models {
		status := statusIconSuccess
		if m.Status != statusSuccess {
			status = statusIconFailed
		}

		if report.IsStressTest {
			requests := "-"
			rps := "-"
			tps := "-"
			p50 := "-"
			p99 := "-"
			errRate := "-"
			if m.Status == statusSuccess {
				requests = fmt.Sprintf("%d", m.TotalRequests)
				rps = fmt.Sprintf("%.1f", m.RequestsPerSec)
				tps = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
				errRate = fmt.Sprintf("%.1f", m.ErrorRate)
			}
			buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				m.ModelID, m.ModelSize, requests, rps, tps, p50, p99, errRate, status))
		} else {
			genToks := "-"
			p50 := "-"
			p99 := "-"
			if m.Status == statusSuccess {
				genToks = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
			}
			buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |\n",
				m.ModelID, m.ModelSize, genToks, p50, p99, m.VRAMEstimate, status))
		}
	}

	for _, m := range report.Models {
		if m.Error != "" {
			buf.WriteString(fmt.Sprintf("\n**Error (%s):** %s\n", m.ModelID, m.Error))
		}
	}

	return rw.writeSection("Model Comparison", buf.String())
}

func (rw *ReportWriter) close() error {
	duration := time.Since(rw.startTime)
	_, _ = fmt.Fprintf(rw.file, "\n---\n\n")
	_, _ = fmt.Fprintf(rw.file, "*Total Duration: %s*  \n", duration.Round(time.Second))
	_, _ = fmt.Fprintf(rw.file, "*Generated by LLMKube v%s*\n", Version)
	return rw.file.Close()
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

type gpuMonitor struct {
	metrics  []GPUMetric
	mu       sync.Mutex
	stopChan chan struct{}
	wg       sync.WaitGroup
}

func newGPUMonitor() *gpuMonitor {
	return &gpuMonitor{
		metrics:  make([]GPUMetric, 0),
		stopChan: make(chan struct{}),
	}
}

func (gm *gpuMonitor) start(interval time.Duration) {
	gm.wg.Add(1)
	go func() {
		defer gm.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		if metric := gm.sample(); metric != nil {
			gm.mu.Lock()
			gm.metrics = append(gm.metrics, *metric)
			gm.mu.Unlock()
		}

		for {
			select {
			case <-gm.stopChan:
				return
			case <-ticker.C:
				if metric := gm.sample(); metric != nil {
					gm.mu.Lock()
					gm.metrics = append(gm.metrics, *metric)
					gm.mu.Unlock()
				}
			}
		}
	}()
}

func (gm *gpuMonitor) stop() []GPUMetric {
	close(gm.stopChan)
	gm.wg.Wait()
	gm.mu.Lock()
	defer gm.mu.Unlock()
	return gm.metrics
}

func (gm *gpuMonitor) sample() *GPUMetric {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=memory.used,memory.total,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err == nil {
		return parseNvidiaSMI(string(output))
	}
	return nil
}

func parseNvidiaSMI(output string) *GPUMetric {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return nil
	}

	var totalMemUsed, totalMemTotal, maxUtil, maxTemp, totalPower int
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}

		memUsed, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
		memTotal, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		util, _ := strconv.Atoi(strings.TrimSpace(fields[2]))

		totalMemUsed += memUsed
		totalMemTotal += memTotal
		if util > maxUtil {
			maxUtil = util
		}

		if len(fields) >= 4 {
			temp, _ := strconv.Atoi(strings.TrimSpace(fields[3]))
			if temp > maxTemp {
				maxTemp = temp
			}
		}
		if len(fields) >= 5 {
			power, _ := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
			totalPower += int(power)
		}
	}

	return &GPUMetric{
		Timestamp:     time.Now(),
		MemoryUsedMB:  totalMemUsed,
		MemoryTotalMB: totalMemTotal,
		UtilPercent:   maxUtil,
		TempCelsius:   maxTemp,
		PowerWatts:    totalPower,
	}
}
