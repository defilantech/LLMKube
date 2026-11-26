package controller

import (
	"slices"
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/gguf"
)

func TestResolveServingMode(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		extraArgs []string
		args      []string
		path      string
		want      string
	}{
		{name: "default is chat", want: servingModeChat},
		{name: "explicit spec.mode wins", mode: servingModeEmbedding, want: servingModeEmbedding},
		{
			name:      "explicit spec.mode wins over conflicting flags",
			mode:      servingModeChat,
			extraArgs: []string{"--embedding"},
			want:      servingModeChat,
		},
		{name: "inferred from --embedding", extraArgs: []string{"--embedding", "--pooling", "last"}, want: servingModeEmbedding},
		{
			name:      "rerank inferred even though reranker also passes --embedding",
			extraArgs: []string{"--reranking", "--pooling", "rank", "--embedding"},
			want:      servingModeRerank,
		},
		{name: "inferred from generic Args", args: []string{"--embeddings"}, want: servingModeEmbedding},
		{name: "inferred from endpoint path", path: "/v1/rerank", want: servingModeRerank},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{}
			isvc.Spec.Mode = tc.mode
			isvc.Spec.ExtraArgs = tc.extraArgs
			isvc.Spec.Args = tc.args
			if tc.path != "" {
				isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{Path: tc.path}
			}
			if got := resolveServingMode(isvc); got != tc.want {
				t.Fatalf("resolveServingMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppendModeArgs(t *testing.T) {
	t.Run("embedding leaves pooling to model metadata", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeEmbedding, nil, "", nil)
		if !containsArg(args, "--embedding", "") || slices.Contains(args, "--pooling") {
			t.Fatalf("expected --embedding without --pooling: %v", args)
		}
	})
	t.Run("embedding adds last pooling when the GGUF records none (#2580)", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeEmbedding, nil, gguf.PoolingTypeUnspecified, nil)
		if !containsArg(args, "--pooling", "last") {
			t.Fatalf("expected --pooling last for a GGUF without pooling_type: %v", args)
		}
	})
	t.Run("embedding leaves a recorded pooling type to llama.cpp", func(t *testing.T) {
		for _, pooling := range []string{"mean", "cls", "last", "none"} {
			if args := appendModeArgs(nil, servingModeEmbedding, nil, pooling, nil); slices.Contains(args, "--pooling") {
				t.Fatalf("pooling %q: expected no --pooling, got %v", pooling, args)
			}
		}
	})
	t.Run("extraArgs pooling wins over the default for a GGUF without one", func(t *testing.T) {
		for _, extra := range [][]string{{"--pooling", "mean"}, {"--pooling=cls"}} {
			if args := appendModeArgs(nil, servingModeEmbedding, extra, gguf.PoolingTypeUnspecified, nil); slices.Contains(args, "--pooling") {
				t.Fatalf("extraArgs %v: expected no appended --pooling, got %v", extra, args)
			}
		}
	})
	t.Run("a metadataOverrides pooling_type wins over the default", func(t *testing.T) {
		overrides := []string{"bert.pooling_type=int:1"}
		if args := appendModeArgs(nil, servingModeEmbedding, nil, gguf.PoolingTypeUnspecified, overrides); slices.Contains(args, "--pooling") {
			t.Fatalf("expected no --pooling when an override sets pooling_type, got %v", args)
		}
		classifier := []string{"bert.classifier.pooling_type=int:2"}
		if args := appendModeArgs(nil, servingModeEmbedding, nil, gguf.PoolingTypeUnspecified, classifier); !containsArg(args, "--pooling", "last") {
			t.Fatalf("the classifier's pooling_type is another key; expected --pooling last, got %v", args)
		}
	})
	t.Run("rerank keeps rank pooling for a GGUF without pooling_type", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeRerank, nil, gguf.PoolingTypeUnspecified, nil)
		if !containsArg(args, "--pooling", "rank") || containsArg(args, "--pooling", "last") {
			t.Fatalf("expected only --pooling rank: %v", args)
		}
	})
	t.Run("chat adds nothing for a GGUF without pooling_type", func(t *testing.T) {
		if args := appendModeArgs(nil, servingModeChat, nil, gguf.PoolingTypeUnspecified, nil); len(args) != 0 {
			t.Fatalf("chat mode should append nothing, got %v", args)
		}
	})
	t.Run("rerank adds --reranking, --embedding and rank pooling", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeRerank, nil, "", nil)
		if !containsArg(args, "--reranking", "") || !containsArg(args, "--embedding", "") || !containsArg(args, "--pooling", "rank") {
			t.Fatalf("missing rerank flags: %v", args)
		}
	})
	t.Run("does not duplicate flags already in extraArgs", func(t *testing.T) {
		extra := []string{"--embedding", "--pooling", "cls", "--cache-ram", "0"}
		args := appendModeArgs(nil, servingModeEmbedding, extra, "", nil)
		if len(args) != 0 {
			t.Fatalf("expected no appended flags when extraArgs already set them, got %v", args)
		}
	})
	t.Run("chat adds nothing", func(t *testing.T) {
		if args := appendModeArgs(nil, servingModeChat, nil, "", nil); len(args) != 0 {
			t.Fatalf("chat mode should append nothing, got %v", args)
		}
	})
	t.Run("embedding sets --cache-ram 0", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeEmbedding, nil, "", nil)
		if !containsArg(args, "--cache-ram", "0") {
			t.Fatalf("embedding mode should set --cache-ram 0, got %v", args)
		}
	})
	t.Run("rerank sets --cache-ram 0", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeRerank, nil, "", nil)
		if !containsArg(args, "--cache-ram", "0") {
			t.Fatalf("rerank mode should set --cache-ram 0, got %v", args)
		}
	})
	t.Run("does not override user --cache-ram in extraArgs", func(t *testing.T) {
		extra := []string{"--cache-ram", "512"}
		args := appendModeArgs(nil, servingModeEmbedding, extra, "", nil)
		if containsArg(args, "--cache-ram", "0") {
			t.Fatalf("should not override user --cache-ram, got %v", args)
		}
	})
}
