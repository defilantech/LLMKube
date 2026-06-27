package main

import (
	"errors"
	"os"
	"testing"

	"go.uber.org/zap/zapcore"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty string returns nil", input: "", want: nil},
		{name: "single value", input: "foo", want: []string{"foo"}},
		{name: "two values", input: "foo,bar", want: []string{"foo", "bar"}},
		{name: "values with surrounding spaces", input: " foo , bar ", want: []string{"foo", "bar"}},
		{name: "trailing comma dropped", input: "foo,", want: []string{"foo"}},
		{name: "leading comma dropped", input: ",foo", want: []string{"foo"}},
		{name: "all-whitespace-only entries returns nil", input: " , , ", want: nil},
		{name: "mixed empty and non-empty", input: "a,,b", want: []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCSV(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitCSV(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCSV(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		t.Run("level="+level, func(t *testing.T) {
			logger, err := newLogger(level)
			if err != nil {
				t.Fatalf("newLogger(%q) returned error: %v", level, err)
			}
			if logger == nil {
				t.Fatalf("newLogger(%q) returned nil logger", level)
			}
		})
	}
}

func TestResolveOMLXBin(t *testing.T) {
	origStat := statFunc
	origPaths := defaultOMLXPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultOMLXPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveOMLXBin("/custom/omlx")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/omlx" {
			t.Fatalf("got %q, want /custom/omlx", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultOMLXPaths = []string{"/first/omlx", "/second/omlx"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/omlx" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveOMLXBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/omlx" {
			t.Fatalf("got %q, want /first/omlx", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultOMLXPaths = []string{"/first/omlx", "/second/omlx"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/omlx" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveOMLXBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/omlx" {
			t.Fatalf("got %q, want /second/omlx", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultOMLXPaths = []string{"/nope/omlx"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveOMLXBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestResolveVLLMSwiftBin(t *testing.T) {
	origStat := statFunc
	origPaths := defaultVLLMSwiftPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultVLLMSwiftPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveVLLMSwiftBin("/custom/vllm-swift")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/vllm-swift" {
			t.Fatalf("got %q, want /custom/vllm-swift", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultVLLMSwiftPaths = []string{"/first/vllm-swift", "/second/vllm-swift"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/vllm-swift" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveVLLMSwiftBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/vllm-swift" {
			t.Fatalf("got %q, want /first/vllm-swift", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultVLLMSwiftPaths = []string{"/first/vllm-swift", "/second/vllm-swift"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/vllm-swift" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveVLLMSwiftBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/vllm-swift" {
			t.Fatalf("got %q, want /second/vllm-swift", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultVLLMSwiftPaths = []string{"/nope/vllm-swift"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveVLLMSwiftBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestResolveMLXServerBin(t *testing.T) {
	origStat := statFunc
	origPaths := defaultMLXServerPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultMLXServerPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveMLXServerBin("/custom/mlx-server")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/mlx-server" {
			t.Fatalf("got %q, want /custom/mlx-server", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultMLXServerPaths = []string{"/first/mlx-server", "/second/mlx-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/mlx-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveMLXServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/mlx-server" {
			t.Fatalf("got %q, want /first/mlx-server", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultMLXServerPaths = []string{"/first/mlx-server", "/second/mlx-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/mlx-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveMLXServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/mlx-server" {
			t.Fatalf("got %q, want /second/mlx-server", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultMLXServerPaths = []string{"/nope/mlx-server"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveMLXServerBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

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
