package agent

import (
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func coderAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Role: foremanv1alpha1.AgentRoleCoder}}
}

func TestApplyModelProfile(t *testing.T) {
	t.Run("nil profile is a no-op", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: DefaultProgressConfig}
		applyModelProfile(cfg, coderAgent(), nil)
		if cfg.SystemPrompt != "ROLE" {
			t.Fatalf("prompt changed: %q", cfg.SystemPrompt)
		}
	})
	t.Run("addendum appends after the agent prompt", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: DefaultProgressConfig}
		p := &foremanv1alpha1.ModelProfile{Spec: foremanv1alpha1.ModelProfileSpec{SystemPromptAddendum: "ADD"}}
		applyModelProfile(cfg, coderAgent(), p)
		if cfg.SystemPrompt != "ROLE\n\nADD" {
			t.Fatalf("got %q", cfg.SystemPrompt)
		}
	})
	t.Run("stuckLoop overlay applies when the agent has none", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: DefaultProgressConfig}
		p := &foremanv1alpha1.ModelProfile{Spec: foremanv1alpha1.ModelProfileSpec{
			StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{EditFreeTurnsLimit: 4},
		}}
		applyModelProfile(cfg, coderAgent(), p)
		if cfg.Progress.EditFreeTurnsLimit != 4 {
			t.Fatalf("editfree got %d want 4", cfg.Progress.EditFreeTurnsLimit)
		}
		if cfg.Progress.RepeatedToolThreshold != DefaultProgressConfig.RepeatedToolThreshold {
			t.Fatalf("default clobbered: %d", cfg.Progress.RepeatedToolThreshold)
		}
	})
	t.Run("agent stuckLoop block wins over profile", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: ProgressConfig{EditFreeTurnsLimit: 9}}
		agent := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{
			Role:               foremanv1alpha1.AgentRoleCoder,
			StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{EditFreeTurnsLimit: 9},
		}}
		p := &foremanv1alpha1.ModelProfile{Spec: foremanv1alpha1.ModelProfileSpec{
			StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{EditFreeTurnsLimit: 4},
		}}
		applyModelProfile(cfg, agent, p)
		if cfg.Progress.EditFreeTurnsLimit != 9 {
			t.Fatalf("agent should win, got %d", cfg.Progress.EditFreeTurnsLimit)
		}
	})
	t.Run("restrictReads flows", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: DefaultProgressConfig}
		p := &foremanv1alpha1.ModelProfile{Spec: foremanv1alpha1.ModelProfileSpec{RestrictReadsInForcingPhase: true}}
		applyModelProfile(cfg, coderAgent(), p)
		if !cfg.RestrictReadsInForcingPhase {
			t.Fatal("restrictReads not set")
		}
	})
	t.Run("reviewer keeps edit-free disabled even if the profile sets it", func(t *testing.T) {
		cfg := &LoopConfig{SystemPrompt: "ROLE", Progress: ProgressConfig{EditFreeTurnsLimit: 0}}
		reviewer := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Role: foremanv1alpha1.AgentRoleReviewer}}
		p := &foremanv1alpha1.ModelProfile{Spec: foremanv1alpha1.ModelProfileSpec{
			StuckLoopDetection: &foremanv1alpha1.StuckLoopDetectionSpec{EditFreeTurnsLimit: 4},
		}}
		applyModelProfile(cfg, reviewer, p)
		if cfg.Progress.EditFreeTurnsLimit != 0 {
			t.Fatalf("reviewer edit-free should stay 0, got %d", cfg.Progress.EditFreeTurnsLimit)
		}
	})
}
