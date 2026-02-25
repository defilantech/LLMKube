package main

import (
	"testing"

	"go.uber.org/zap/zapcore"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected zapcore.Level
	}{
		{name: "debug", input: "debug", expected: zapcore.DebugLevel},
		{name: "info", input: "info", expected: zapcore.InfoLevel},
		{name: "warn", input: "warn", expected: zapcore.WarnLevel},
		{name: "warning", input: "warning", expected: zapcore.WarnLevel},
		{name: "error", input: "error", expected: zapcore.ErrorLevel},
		{name: "empty defaults info", input: "", expected: zapcore.InfoLevel},
		{name: "unknown defaults info", input: "unknown", expected: zapcore.InfoLevel},
		{name: "mixed case debug", input: "DEBUG", expected: zapcore.DebugLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}
