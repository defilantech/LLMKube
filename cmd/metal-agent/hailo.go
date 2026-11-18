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

package main

import (
	"path/filepath"

	"go.uber.org/zap"
)

// hailoDeviceGlob matches the device nodes the Hailo PCIe driver creates.
const hailoDeviceGlob = "/dev/hailo*"

// globDevices is filepath.Glob, replaced in tests.
var globDevices = filepath.Glob

// logHailoDevices logs whether a Hailo device node is present.
func logHailoDevices(logger *zap.SugaredLogger) {
	if devices, _ := globDevices(hailoDeviceGlob); len(devices) > 0 {
		logger.Infow("Hailo device detected", "devices", devices)
	} else {
		logger.Infow("no Hailo device found", "checked", hailoDeviceGlob)
	}
}
