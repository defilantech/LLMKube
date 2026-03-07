package main

import (
	"errors"
	"os"
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

func TestResolveLlamaServerBin(t *testing.T) {
	// Save and restore the original statFunc and defaultLlamaServerPaths
	origStat := statFunc
	origPaths := defaultLlamaServerPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultLlamaServerPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveLlamaServerBin("/custom/path/llama-server")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/path/llama-server" {
			t.Fatalf("got %q, want /custom/path/llama-server", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/llama-server" {
			t.Fatalf("got %q, want /first/llama-server", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/llama-server" {
			t.Fatalf("got %q, want /second/llama-server", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/nope/llama-server"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveLlamaServerBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
