package main

import "testing"

func TestMapGateVerdict(t *testing.T) {
	cases := []struct {
		verdict           string
		wantPass, wantRan bool
	}{
		{"GATE-PASS", true, true},
		{"GATE-FAIL", false, true},
		{"GATE-ERROR", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		pass, ran := mapGateVerdict(c.verdict)
		if pass != c.wantPass || ran != c.wantRan {
			t.Errorf("mapGateVerdict(%q) = (%v,%v) want (%v,%v)", c.verdict, pass, ran, c.wantPass, c.wantRan)
		}
	}
}
