package mcp

import (
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	cases := []struct {
		tool  string
		allow []string
		want  bool
	}{
		{"x", nil, true}, {"x", []string{"*"}, true},
		{"x", []string{"x"}, true}, {"x", []string{"y"}, false},
	}
	for _, c := range cases {
		if got := allowed(c.tool, c.allow); got != c.want {
			t.Errorf("allowed(%q,%v)=%v want %v", c.tool, c.allow, got, c.want)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.CallTimeout != 30*time.Second {
		t.Errorf("CallTimeout default = %v, want 30s", o.CallTimeout)
	}
	if o.MaxResultBytes != 32768 {
		t.Errorf("MaxResultBytes default = %d, want 32768", o.MaxResultBytes)
	}
}
