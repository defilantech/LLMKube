package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// containsArg reports whether args contains the given flag. When value is
// non-empty, it also requires the immediately following entry to equal value
// (i.e. `--flag value` as separate slice elements, which is how BuildArgs
// emits everything).
func containsArg(args []string, flag, value string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if value == "" {
			return true
		}
		if i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// ptrString, ptrBool, ptrInt32 are local helpers so tests read naturally.
func ptrBool(b bool) *bool          { return &b }
func ptrFloat64(f float64) *float64 { return &f }
func ptrInt32(i int32) *int32       { return &i }
func ptrString(s string) *string    { return &s }

type FlagCheck struct {
	flag  string
	value string
}

type RuntimeBuildArgsTestCase struct {
	// contains is a slice of flag/value pairs ("" value means "flag must be
	// present as a bare toggle").
	contains []FlagCheck
	// notContains is just a list of flags that must NOT appear anywhere in args.
	notContains []string
	model       *inferencev1alpha1.Model
	name        string
	spec        *inferencev1alpha1.InferenceServiceSpec
}

func TestResolveGPUCount(t *testing.T) {
	cases := []struct {
		expected int32
		isvc     *inferencev1alpha1.InferenceService
		model    *inferencev1alpha1.Model
		name     string
	}{
		{
			expected: 1,
			isvc:     &inferencev1alpha1.InferenceService{},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count: 1,
						},
					},
				},
			},
			name: "model.Spec.Hardware.GPU.Count set resolve GPU count",
		},
		{
			expected: 1,
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 1,
					},
				},
			},
			model: &inferencev1alpha1.Model{},
			name:  "isvc.Spec.Resources.GPU set resolve GPU count",
		},
		{
			expected: 1,
			isvc: &inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Resources: &inferencev1alpha1.InferenceResourceRequirements{
						GPU: 2,
					},
				},
			},
			model: &inferencev1alpha1.Model{
				Spec: inferencev1alpha1.ModelSpec{
					Hardware: &inferencev1alpha1.HardwareSpec{
						GPU: &inferencev1alpha1.GPUSpec{
							Count: 1,
						},
					},
				},
			},
			name: "model.Spec.Hardware.GPU.Count have precedence over isvc.Spec.Resources.GPU",
		},
		{
			expected: 0,
			isvc:     &inferencev1alpha1.InferenceService{},
			model:    &inferencev1alpha1.Model{},
			name:     "no GPU set resolve to 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := resolveGPUCount(tc.isvc, tc.model)
			if actual != tc.expected {
				t.Errorf("expected %d GPU count, got: %d", tc.expected, actual)
			}
		})
	}
}
