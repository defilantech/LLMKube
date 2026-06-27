package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
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
	t.Run("embedding adds --embedding and default pooling", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeEmbedding, nil)
		if !containsArg(args, "--embedding", "") || !containsArg(args, "--pooling", "last") {
			t.Fatalf("missing embedding flags: %v", args)
		}
	})
	t.Run("rerank adds --reranking, --embedding and rank pooling", func(t *testing.T) {
		args := appendModeArgs(nil, servingModeRerank, nil)
		if !containsArg(args, "--reranking", "") || !containsArg(args, "--embedding", "") || !containsArg(args, "--pooling", "rank") {
			t.Fatalf("missing rerank flags: %v", args)
		}
	})
	t.Run("does not duplicate flags already in extraArgs", func(t *testing.T) {
		extra := []string{"--embedding", "--pooling", "cls"}
		args := appendModeArgs(nil, servingModeEmbedding, extra)
		if len(args) != 0 {
			t.Fatalf("expected no appended flags when extraArgs already set them, got %v", args)
		}
	})
	t.Run("chat adds nothing", func(t *testing.T) {
		if args := appendModeArgs(nil, servingModeChat, nil); len(args) != 0 {
			t.Fatalf("chat mode should append nothing, got %v", args)
		}
	})
}
