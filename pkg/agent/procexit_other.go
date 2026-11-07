//go:build !darwin

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
	"errors"
)

// errProcessExitWatchUnsupported reports a platform without a process exit
// watch for non-children. The agent only runs on macOS; this exists so the
// package builds and its tests run on Linux CI, where the health probe stays
// the detector for an adopted engine's exit.
var errProcessExitWatchUnsupported = errors.New("watching a non-child process for its exit is unsupported here")

func watchProcessExit(context.Context, int) (*childExit, error) {
	return nil, errProcessExitWatchUnsupported
}
