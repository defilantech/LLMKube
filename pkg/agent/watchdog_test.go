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
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func defaultTestConfig() MemoryWatchdogConfig {
	return MemoryWatchdogConfig{
		Interval:          50 * time.Millisecond,
		WarningThreshold:  0.20,
		CriticalThreshold: 0.10,
	}
}

func TestWatchdog_NormalPressure(t *testing.T) {
	provider := &mockMemoryProvider{
		totalBytes:     64 * 1024 * 1024 * 1024,
		availableBytes: 32 * 1024 * 1024 * 1024, // 50% available — normal
		wiredBytes:     8 * 1024 * 1024 * 1024,
		processRSS:     map[int]uint64{100: 4 * 1024 * 1024 * 1024},
	}

	var mu sync.Mutex
	callCount := 0
	onPressure := func(_ MemoryPressureLevel, _ MemoryStats) {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	procs := func() []processMemInfo {
		return []processMemInfo{{Name: "test-model", PID: 100}}
	}

	w := NewMemoryWatchdog(provider, procs, onPressure, defaultTestConfig(), testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 0 {
		t.Errorf("onPressure called %d times, want 0 (normal pressure)", callCount)
	}
}

func TestWatchdog_PressureLevels(t *testing.T) {
	total := uint64(64 * 1024 * 1024 * 1024)

	tests := []struct {
		name          string
		availFraction float64
		expected      MemoryPressureLevel
	}{
		{"warning at 15% available", 0.15, MemoryPressureWarning},
		{"critical at 5% available", 0.05, MemoryPressureCritical},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &mockMemoryProvider{
				totalBytes:     total,
				availableBytes: uint64(float64(total) * tt.availFraction),
				wiredBytes:     10 * 1024 * 1024 * 1024,
			}

			var mu sync.Mutex
			var captured []MemoryPressureLevel
			onPressure := func(level MemoryPressureLevel, _ MemoryStats) {
				mu.Lock()
				captured = append(captured, level)
				mu.Unlock()
			}

			procs := func() []processMemInfo { return nil }
			w := NewMemoryWatchdog(provider, procs, onPressure, defaultTestConfig(), testLogger())

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			w.Run(ctx)

			mu.Lock()
			defer mu.Unlock()
			if len(captured) == 0 {
				t.Fatalf("expected onPressure to be called at %v level", tt.expected)
			}
			for _, level := range captured {
				if level != tt.expected {
					t.Errorf("got pressure level %v, want %v", level, tt.expected)
				}
			}
		})
	}
}

func TestWatchdog_ProcessRSSTracking(t *testing.T) {
	provider := &mockMemoryProvider{
		totalBytes:     64 * 1024 * 1024 * 1024,
		availableBytes: 32 * 1024 * 1024 * 1024,
		wiredBytes:     8 * 1024 * 1024 * 1024,
		processRSS: map[int]uint64{
			100: 4 * 1024 * 1024 * 1024,
			200: 2 * 1024 * 1024 * 1024,
		},
	}

	procs := func() []processMemInfo {
		return []processMemInfo{
			{Name: "model-a", PID: 100},
			{Name: "model-b", PID: 200},
		}
	}

	w := NewMemoryWatchdog(provider, procs, nil, defaultTestConfig(), testLogger())

	// Run a single sample and check stats
	w.sample()

	// Metrics should have been set — verify via the gauge
	rssA := processRSSBytes.WithLabelValues("model-a")
	rssB := processRSSBytes.WithLabelValues("model-b")

	var mA, mB dto.Metric
	if err := rssA.Write(&mA); err != nil {
		t.Fatalf("failed to read model-a RSS metric: %v", err)
	}
	if err := rssB.Write(&mB); err != nil {
		t.Fatalf("failed to read model-b RSS metric: %v", err)
	}

	if mA.GetGauge().GetValue() != float64(4*1024*1024*1024) {
		t.Errorf("model-a RSS = %f, want %d", mA.GetGauge().GetValue(), 4*1024*1024*1024)
	}
	if mB.GetGauge().GetValue() != float64(2*1024*1024*1024) {
		t.Errorf("model-b RSS = %f, want %d", mB.GetGauge().GetValue(), 2*1024*1024*1024)
	}
}

func TestWatchdog_NilOnPressureDoesNotPanic(t *testing.T) {
	total := uint64(64 * 1024 * 1024 * 1024)
	provider := &mockMemoryProvider{
		totalBytes:     total,
		availableBytes: uint64(float64(total) * 0.05), // critical
		wiredBytes:     20 * 1024 * 1024 * 1024,
	}

	procs := func() []processMemInfo { return nil }
	w := NewMemoryWatchdog(provider, procs, nil, defaultTestConfig(), testLogger())

	// Should not panic even with nil onPressure
	w.sample()
}

func TestComputePressure_Thresholds(t *testing.T) {
	cfg := defaultTestConfig()
	w := &MemoryWatchdog{config: cfg}

	tests := []struct {
		name     string
		avail    float64
		expected MemoryPressureLevel
	}{
		{"50% available — normal", 0.50, MemoryPressureNormal},
		{"25% available — normal", 0.25, MemoryPressureNormal},
		{"21% available — normal (above warning)", 0.21, MemoryPressureNormal},
		{"19% available — warning", 0.19, MemoryPressureWarning},
		{"15% available — warning", 0.15, MemoryPressureWarning},
		{"11% available — warning (above critical)", 0.11, MemoryPressureWarning},
		{"9% available — critical", 0.09, MemoryPressureCritical},
		{"5% available — critical", 0.05, MemoryPressureCritical},
		{"0% available — critical", 0.00, MemoryPressureCritical},
	}

	total := uint64(64 * 1024 * 1024 * 1024)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := MemoryStats{
				TotalMemory:     total,
				AvailableMemory: uint64(float64(total) * tt.avail),
			}
			got := w.computePressure(stats)
			if got != tt.expected {
				t.Errorf("computePressure(avail=%.0f%%) = %v, want %v",
					tt.avail*100, got, tt.expected)
			}
		})
	}
}

func TestMemoryPressureLevel_String(t *testing.T) {
	tests := []struct {
		level    MemoryPressureLevel
		expected string
	}{
		{MemoryPressureNormal, "normal"},
		{MemoryPressureWarning, "warning"},
		{MemoryPressureCritical, "critical"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.expected {
			t.Errorf("MemoryPressureLevel(%d).String() = %q, want %q",
				tt.level, got, tt.expected)
		}
	}
}
