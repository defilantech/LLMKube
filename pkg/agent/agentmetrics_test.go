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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestAgentMetricsRegistered(t *testing.T) {
	collectors := []struct {
		name      string
		collector prometheus.Collector
	}{
		{"llmkube_metal_agent_managed_processes", managedProcesses},
		{"llmkube_metal_agent_process_healthy", processHealthy},
		{"llmkube_metal_agent_process_restarts_total", processRestarts},
		{"llmkube_metal_agent_health_check_duration_seconds", healthCheckDuration},
		{"llmkube_metal_agent_memory_budget_bytes", memoryBudgetBytes},
		{"llmkube_metal_agent_memory_estimated_bytes", memoryEstimatedBytes},
		{"llmkube_metal_agent_system_memory_available_bytes", systemMemoryAvailableBytes},
		{"llmkube_metal_agent_system_memory_wired_bytes", systemMemoryWiredBytes},
		{"llmkube_metal_agent_process_rss_bytes", processRSSBytes},
		{"llmkube_metal_agent_memory_pressure_level", memoryPressureLevelGauge},
		{"llmkube_metal_agent_evictions_total", evictionsTotal},
	}

	for _, c := range collectors {
		t.Run(c.name, func(t *testing.T) {
			err := AgentRegistry.Register(c.collector)
			if err == nil {
				t.Errorf("metric %q was not already registered — init() did not register it", c.name)
				AgentRegistry.Unregister(c.collector)
			} else if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				t.Errorf("unexpected error registering %q: %v", c.name, err)
			}
		})
	}
}

func TestManagedProcessesGauge(t *testing.T) {
	managedProcesses.Set(3)

	var m dto.Metric
	if err := managedProcesses.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 3 {
		t.Errorf("expected gauge value 3, got %f", m.GetGauge().GetValue())
	}

	managedProcesses.Set(0)
	if err := managedProcesses.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 0 {
		t.Errorf("expected gauge value 0, got %f", m.GetGauge().GetValue())
	}
}

func TestProcessHealthyGaugeVec(t *testing.T) {
	processHealthy.WithLabelValues("test-model", "default").Set(1)

	var m dto.Metric
	if err := processHealthy.WithLabelValues("test-model", "default").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 1 {
		t.Errorf("expected gauge value 1, got %f", m.GetGauge().GetValue())
	}

	processHealthy.WithLabelValues("test-model", "default").Set(0)
	if err := processHealthy.WithLabelValues("test-model", "default").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 0 {
		t.Errorf("expected gauge value 0, got %f", m.GetGauge().GetValue())
	}
}

func TestProcessRestartsCounter(t *testing.T) {
	processRestarts.WithLabelValues("restart-test", "default").Inc()
	processRestarts.WithLabelValues("restart-test", "default").Inc()

	var m dto.Metric
	if err := processRestarts.WithLabelValues("restart-test", "default").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetCounter().GetValue() < 2 {
		t.Errorf("expected counter >= 2, got %f", m.GetCounter().GetValue())
	}
}

func TestHealthCheckDurationHistogram(t *testing.T) {
	healthCheckDuration.WithLabelValues("hc-test", "default").Observe(0.05)

	observer, err := healthCheckDuration.GetMetricWithLabelValues("hc-test", "default")
	if err != nil {
		t.Fatalf("failed to get metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected sample count > 0 after observation")
	}
}

func TestMemoryBudgetGauge(t *testing.T) {
	memoryBudgetBytes.Set(16_000_000_000)

	var m dto.Metric
	if err := memoryBudgetBytes.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 16_000_000_000 {
		t.Errorf("expected gauge value 16000000000, got %f", m.GetGauge().GetValue())
	}
}

func TestMemoryEstimatedBytesGaugeVec(t *testing.T) {
	memoryEstimatedBytes.WithLabelValues("mem-test", "default").Set(4_000_000_000)

	var m dto.Metric
	if err := memoryEstimatedBytes.WithLabelValues("mem-test", "default").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 4_000_000_000 {
		t.Errorf("expected gauge value 4000000000, got %f", m.GetGauge().GetValue())
	}
}
