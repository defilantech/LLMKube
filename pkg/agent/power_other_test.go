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
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestApplePowerSampler_OtherIsNoOp guards the cross-platform contract: on
// non-Darwin builds the sampler must construct cleanly, satisfy the runner
// interface, and exit Run promptly without doing any work. CI runs on Linux
// and would silently lose the build-tag gate if power_other.go were ever
// removed or changed to call into Darwin-only code.
func TestApplePowerSampler_OtherIsNoOp(t *testing.T) {
	s := NewApplePowerSampler("/anything/at/all", 5*time.Second, zap.NewNop().Sugar())
	if s == nil {
		t.Fatal("NewApplePowerSampler returned nil on non-darwin")
	}

	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly on non-darwin (should be a no-op)")
	}
}
