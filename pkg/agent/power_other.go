//go:build !darwin

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

// DefaultPowermetricsBin keeps the symbol available cross-platform so cmd/
// can reference it in flag defaults without build tags. It has no meaning on
// non-Darwin systems.
const DefaultPowermetricsBin = "/usr/bin/powermetrics"

// DefaultApplePowerInterval mirrors the Darwin constant for cross-platform
// flag defaults. powermetrics doesn't exist outside macOS.
const DefaultApplePowerInterval = time.Second

// ApplePowerSampler is a no-op stub for non-Darwin builds. The Metal agent
// only runs in production on macOS, but the package compiles cross-platform
// so unit tests can run in CI on Linux.
type ApplePowerSampler struct{}

// NewApplePowerSampler returns a no-op sampler on non-Darwin.
func NewApplePowerSampler(_ string, _ time.Duration, _ *zap.SugaredLogger) *ApplePowerSampler {
	return &ApplePowerSampler{}
}

// Run returns immediately on non-Darwin builds. Apple power data is only
// available via macOS powermetrics.
func (s *ApplePowerSampler) Run(_ context.Context) {}
