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
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestNewLicenseCommand(t *testing.T) {
	cmd := NewLicenseCommand()

	if cmd.Use != "license" {
		t.Errorf("Use = %q, want %q", cmd.Use, "license")
	}

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["check"] {
		t.Error("Missing 'check' subcommand")
	}
	if !subcommands["list"] {
		t.Error("Missing 'list' subcommand")
	}
}

func TestNewLicenseCheckCommand(t *testing.T) {
	cmd := NewLicenseCheckCommand()

	if cmd.Use != "check MODEL_NAME" {
		t.Errorf("Use = %q, want %q", cmd.Use, "check MODEL_NAME")
	}

	if cmd.Flags().Lookup("namespace") == nil {
		t.Error("Missing --namespace flag")
	}

	ns := cmd.Flags().Lookup("namespace")
	if ns.DefValue != "default" {
		t.Errorf("namespace default = %q, want %q", ns.DefValue, "default")
	}

	if ns.Shorthand != "n" {
		t.Errorf("namespace shorthand = %q, want %q", ns.Shorthand, "n")
	}
}

func TestRunLicenseList(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runLicenseList()

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "ID") {
		t.Error("output should contain header")
	}
	if !strings.Contains(output, "apache-2.0") {
		t.Error("output should contain apache-2.0")
	}
	if !strings.Contains(output, "mit") {
		t.Error("output should contain mit")
	}
	if !strings.Contains(output, "gemma") {
		t.Error("output should contain gemma")
	}
}
