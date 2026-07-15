package agent

import (
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func i32p(v int32) *int32 { return &v }

func TestEffectiveMaxEnvtestIterations(t *testing.T) {
	cases := []struct {
		name string
		in   *int32
		want int
	}{
		{"nil defaults to 1", nil, 1},
		{"explicit 0 opts out", i32p(0), 0},
		{"explicit N honored", i32p(3), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &foremanv1alpha1.Agent{}
			a.Spec.MaxEnvtestIterations = tc.in
			if got := effectiveMaxEnvtestIterations(a); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	t.Run("nil agent defaults to 1", func(t *testing.T) {
		if got := effectiveMaxEnvtestIterations(nil); got != 1 {
			t.Fatalf("got %d want 1", got)
		}
	})
}
