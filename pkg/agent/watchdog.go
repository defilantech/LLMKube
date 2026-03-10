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
	"context"
	"time"

	"go.uber.org/zap"
)

// MemoryWatchdogConfig holds configuration for the MemoryWatchdog.
type MemoryWatchdogConfig struct {
	Interval          time.Duration
	WarningThreshold  float64 // available fraction below which we warn (e.g. 0.20)
	CriticalThreshold float64 // available fraction below which we go critical (e.g. 0.10)
}

// MemoryStats holds a point-in-time snapshot of system and process memory.
type MemoryStats struct {
	TotalMemory     uint64
	AvailableMemory uint64
	WiredMemory     uint64
	ProcessRSS      map[string]uint64 // keyed by process name
	TotalRSS        uint64
	PressureLevel   MemoryPressureLevel
}

// MemoryWatchdog periodically samples system memory and per-process RSS,
// computes a pressure level, and updates Prometheus metrics.
type MemoryWatchdog struct {
	provider   MemoryProvider
	processes  func() []processMemInfo
	onPressure func(level MemoryPressureLevel, stats MemoryStats)
	config     MemoryWatchdogConfig
	logger     *zap.SugaredLogger
}

// processMemInfo is the minimal info needed to sample a process's RSS.
type processMemInfo struct {
	Name string
	PID  int
}

// NewMemoryWatchdog creates a new watchdog.
func NewMemoryWatchdog(
	provider MemoryProvider,
	processes func() []processMemInfo,
	onPressure func(level MemoryPressureLevel, stats MemoryStats),
	config MemoryWatchdogConfig,
	logger *zap.SugaredLogger,
) *MemoryWatchdog {
	return &MemoryWatchdog{
		provider:   provider,
		processes:  processes,
		onPressure: onPressure,
		config:     config,
		logger:     logger,
	}
}

// Run starts the watchdog loop. It blocks until ctx is cancelled.
func (w *MemoryWatchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sample()
		}
	}
}

// sample performs one memory check cycle.
func (w *MemoryWatchdog) sample() {
	stats := MemoryStats{
		ProcessRSS: make(map[string]uint64),
	}

	total, err := w.provider.TotalMemory()
	if err != nil {
		w.logger.Warnw("watchdog: failed to get total memory", "error", err)
		return
	}
	stats.TotalMemory = total

	avail, err := w.provider.AvailableMemory()
	if err != nil {
		w.logger.Warnw("watchdog: failed to get available memory", "error", err)
	} else {
		stats.AvailableMemory = avail
		systemMemoryAvailableBytes.Set(float64(avail))
	}

	wired, err := w.provider.WiredMemory()
	if err != nil {
		w.logger.Debugw("watchdog: failed to get wired memory", "error", err)
	} else {
		stats.WiredMemory = wired
		systemMemoryWiredBytes.Set(float64(wired))
	}

	// Sample per-process RSS
	for _, p := range w.processes() {
		rss, rssErr := w.provider.ProcessRSS(p.PID)
		if rssErr != nil {
			w.logger.Debugw("watchdog: failed to get process RSS",
				"name", p.Name, "pid", p.PID, "error", rssErr)
			continue
		}
		stats.ProcessRSS[p.Name] = rss
		stats.TotalRSS += rss
		processRSSBytes.WithLabelValues(p.Name).Set(float64(rss))
	}

	// Compute pressure level
	stats.PressureLevel = w.computePressure(stats)
	memoryPressureLevelGauge.Set(float64(stats.PressureLevel))

	if stats.PressureLevel >= MemoryPressureWarning {
		w.logger.Warnw("memory pressure detected",
			"level", stats.PressureLevel.String(),
			"available", formatMemory(stats.AvailableMemory),
			"total", formatMemory(stats.TotalMemory),
			"wired", formatMemory(stats.WiredMemory),
			"totalRSS", formatMemory(stats.TotalRSS),
		)
		if w.onPressure != nil {
			w.onPressure(stats.PressureLevel, stats)
		}
	}
}

// computePressure determines the pressure level from available/total ratio.
func (w *MemoryWatchdog) computePressure(stats MemoryStats) MemoryPressureLevel {
	if stats.TotalMemory == 0 {
		return MemoryPressureNormal
	}
	availFraction := float64(stats.AvailableMemory) / float64(stats.TotalMemory)

	if availFraction < w.config.CriticalThreshold {
		return MemoryPressureCritical
	}
	if availFraction < w.config.WarningThreshold {
		return MemoryPressureWarning
	}
	return MemoryPressureNormal
}
