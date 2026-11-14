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
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestEnsureProcess_RopeScalingEditRespawns is #2410: ropeScaling becomes
// engine arguments (--rope-scaling, --rope-scale, --yarn-orig-ctx), so an
// edit to it must restart the engine like any other argument.
func TestEnsureProcess_RopeScalingEditRespawns(t *testing.T) {
	f := newRestartFixture(t, "rope-edit")
	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.RopeScaling = &inferencev1alpha1.RopeScalingSpec{Type: "yarn", Factor: "2.0"}
	})
	if err := f.agent.ensureProcess(context.Background(), changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if got := f.exec.starts(); got != 1 {
		t.Fatalf("starts after a ropeScaling edit = %d, want 1 (the engine restarted with the new arguments)", got)
	}
	if p := f.tracked(); p == nil || p.SpecHash != computeSpecHash(changed) {
		t.Errorf("tracked process = %+v, want one started with the new spec", p)
	}
}

// TestEnsureProcess_ReplicasAboveOneDoesNotRespawn: a Mac runs one engine per
// InferenceService whatever spec.replicas says, so raising replicas from 1 to
// 2 must not restart the engine it already runs.
func TestEnsureProcess_ReplicasAboveOneDoesNotRespawn(t *testing.T) {
	f := newRestartFixture(t, "replicas-two")
	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.Replicas = ptr.To(int32(2))
	})
	if err := f.agent.ensureProcess(context.Background(), changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts after replicas 1 -> 2 = %d, want 0 (the running engine kept)", got)
	}
}

// TestEnsureProcess_RefreshesEvictionPolicyWithoutRespawn: priority and
// evictionProtection do not change the engine's arguments, so editing them
// keeps the engine, but the memory-pressure selector must see the new values
// at once. The memory-pressure runbook tells users to add evictionProtection
// to a running service.
func TestEnsureProcess_RefreshesEvictionPolicyWithoutRespawn(t *testing.T) {
	f := newRestartFixture(t, "protect-me")
	f.agent.mu.Lock()
	f.agent.processes["default/other"] = &ManagedProcess{
		Name: "other", Namespace: "default", PID: 2222, Port: 18081, Healthy: true,
		Runtime: runtimeLlamaServer, Priority: "high",
	}
	f.agent.mu.Unlock()
	rss := map[string]uint64{}

	target, _ := pickEvictionTarget(f.agent.processes, rss)
	if target == nil || target.Name != "protect-me" {
		t.Fatalf("eviction target before the edit = %+v, want protect-me (normal priority, unprotected)", target)
	}

	changed := f.updateISVC(t, func(isvc *inferencev1alpha1.InferenceService) {
		isvc.Spec.EvictionProtection = ptr.To(true)
		isvc.Spec.Priority = "critical"
	})
	if err := f.agent.ensureProcess(context.Background(), changed); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	if got := f.exec.starts(); got != 0 {
		t.Fatalf("starts after an eviction-policy edit = %d, want 0", got)
	}
	f.agent.mu.RLock()
	target, _ = pickEvictionTarget(f.agent.processes, rss)
	f.agent.mu.RUnlock()
	if target == nil || target.Name != "other" {
		t.Errorf("eviction target after protecting protect-me = %+v, want other", target)
	}
	if p := f.tracked(); p == nil || !p.EvictionProtection || p.Priority != "critical" {
		t.Errorf("tracked process = %+v, want EvictionProtection true and Priority critical", p)
	}
}

// TestComputeSpecHash_UnchangedForExistingSpecs pins the hash of specs that
// set neither ropeScaling nor replicas above 1 to the values the agent
// computed before #2410. An adopted engine keeps the hash its record holds, so
// a changed hash would restart every engine on the Mac when the agent is
// upgraded.
func TestComputeSpecHash_UnchangedForExistingSpecs(t *testing.T) {
	isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{
		ModelRef: "qwen3-8b", Replicas: ptr.To(int32(1)), ContextSize: ptr.To(int32(8192)),
		FlashAttention: ptr.To(true), ExtraArgs: []string{"--jinja"},
	}}
	const replicasOne = "c2a2e182e3f487cfe478242b4fb78f75434eafc1580a49637de67b4537a2a856"
	if got, want := computeSpecHash(isvc), replicasOne; got != want {
		t.Errorf("hash with replicas 1 = %s, want %s", got, want)
	}
	isvc.Spec.Replicas = nil
	const replicasUnset = "cb19f05c8f42987a75d6c6cb0ebc664182146c16a1441c222715a7d86391a852"
	if got, want := computeSpecHash(isvc), replicasUnset; got != want {
		t.Errorf("hash with replicas unset = %s, want %s", got, want)
	}
}

// TestComputeSpecHash_CoversEveryExecutorConfigField is the drift guard the
// computeSpecHash comment asks for: set each InferenceServiceSpec field in
// turn, and whenever buildExecutorConfig produces a different engine
// configuration for it, the spec hash must change too. A new spec field that
// reaches the engine's arguments without a hash entry fails here, as
// ropeScaling did (#2410).
func TestComputeSpecHash_CoversEveryExecutorConfigField(t *testing.T) {
	model := &inferencev1alpha1.Model{}
	model.Name = "m"
	base := &inferencev1alpha1.InferenceService{}
	base.Name, base.Namespace = "svc", "default"
	baseCfg := comparableExecutorConfig(buildExecutorConfig(base, model, executorBaseConfig{}))
	baseHash := computeSpecHash(base)

	specType := reflect.TypeOf(inferencev1alpha1.InferenceServiceSpec{})
	for i := range specType.NumField() {
		field := specType.Field(i)
		mut := base.DeepCopy()
		fv := reflect.ValueOf(&mut.Spec).Elem().Field(i)
		if !setNonZero(fv) {
			t.Logf("skipped %s: could not build a non-zero %s", field.Name, field.Type)
			continue
		}
		if reflect.DeepEqual(baseCfg, comparableExecutorConfig(buildExecutorConfig(mut, model, executorBaseConfig{}))) {
			continue
		}
		if computeSpecHash(mut) == baseHash {
			t.Errorf("spec.%s changes the engine configuration but not the spec hash: add it to computeSpecHash",
				field.Name)
		}
	}
}

// comparableExecutorConfig zeroes the func fields of cfg, which DeepEqual
// never reports equal, so two configurations compare by their data.
func comparableExecutorConfig(cfg ExecutorConfig) ExecutorConfig {
	v := reflect.ValueOf(&cfg).Elem()
	for i := range v.NumField() {
		if v.Field(i).Kind() == reflect.Func && v.Field(i).CanSet() {
			v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
		}
	}
	return cfg
}

// setNonZero sets v to a non-zero value of its type, recursing into pointers,
// slices, maps and structs. It reports false for a type it cannot build.
func setNonZero(v reflect.Value) bool {
	if !v.CanSet() {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x1")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(3)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(3)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		elem := reflect.New(v.Type().Elem())
		if !setNonZero(elem.Elem()) {
			return false
		}
		v.Set(elem)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		if !setNonZero(s.Index(0)) {
			return false
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		if !setNonZero(k) || !setNonZero(e) {
			return false
		}
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		set := false
		for i := range v.NumField() {
			if setNonZero(v.Field(i)) {
				set = true
			}
		}
		return set
	default:
		return false
	}
	return true
}

// TestSameEngineSpec covers which stored hashes count as the engine isvc asks
// for: replicas unset, 1 and above 1 are one engine; anything else that
// changes the hash is not.
func TestSameEngineSpec(t *testing.T) {
	spec := func(replicas *int32, ctx int32) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "m", Replicas: replicas, ContextSize: ptr.To(ctx)}}
	}
	unset := computeSpecHash(spec(nil, 4096))
	one := computeSpecHash(spec(ptr.To(int32(1)), 4096))
	tests := []struct {
		name   string
		stored string
		isvc   *inferencev1alpha1.InferenceService
		want   bool
	}{
		{"unset to 1", unset, spec(ptr.To(int32(1)), 4096), true},
		{"1 to unset", one, spec(nil, 4096), true},
		{"unset to 3", unset, spec(ptr.To(int32(3)), 4096), true},
		{"1 to 2", one, spec(ptr.To(int32(2)), 4096), true},
		{"started at 2, edited to 3", computeSpecHash(spec(ptr.To(int32(2)), 4096)), spec(ptr.To(int32(3)), 4096), true},
		{"1 to 0", one, spec(ptr.To(int32(0)), 4096), false},
		{"context changed", one, spec(ptr.To(int32(1)), 8192), false},
		{"no stored hash", "", spec(nil, 4096), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameEngineSpec(tc.stored, tc.isvc); got != tc.want {
				t.Errorf("sameEngineSpec = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHandleMemoryPressure_RefreshDuringTickIsNotARace: refreshEvictionPolicy
// writes a running process's priority and protection under a.mu while a
// pressure tick reads them after releasing it. The tick works on copies, so
// under -race (the macOS CI job runs this package with it) the two never
// touch the same memory. With every process protected, each tick reads the
// protection of all of them and evicts none.
func TestHandleMemoryPressure_RefreshDuringTickIsNotARace(t *testing.T) {
	isvc := newPressureTestISvc("svc-a", "low")
	isvc.Spec.EvictionProtection = ptr.To(true)
	a := newPressureTestAgent(t, isvc, newPressureTestISvc("svc-b", "low"))
	a.config.EvictionEnabled = true
	a.executors[runtimeLlamaServer] = stubExecutor{}
	for _, name := range []string{"svc-a", "svc-b"} {
		a.processes["default/"+name] = &ManagedProcess{
			Name: name, Namespace: "default", ModelPath: "/m.gguf", Priority: "low",
			EvictionProtection: true, Runtime: runtimeLlamaServer,
		}
	}
	stats := MemoryStats{TotalMemory: 100 << 30, TotalRSS: 80 << 30,
		ProcessRSS: map[string]uint64{"svc-a": 40 << 30, "svc-b": 40 << 30}}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				a.refreshEvictionPolicy("default/svc-a", isvc)
			}
		}
	}()
	for range 200 {
		a.handleMemoryPressure(context.Background(), MemoryPressureCritical, stats)
	}
	close(stop)
	<-done

	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.processes) != 2 {
		t.Errorf("managed processes = %d, want 2 (every process is protected)", len(a.processes))
	}
}
