package mcp

import "testing"

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
	if o.CallTimeout <= 0 || o.MaxResultBytes <= 0 {
		t.Fatalf("defaults not applied: %+v", o)
	}
}
