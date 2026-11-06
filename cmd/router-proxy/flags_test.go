/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"flag"
	"io"
	"slices"
	"testing"
	"time"
)

// TestShutdownTimeoutFlagParses drives the real flag registration, so it pins
// both halves of the ModelRouter spec.proxy.drainTimeout contract: the proxy
// keeps a 30s drain when the controller renders no flag, and it accepts the
// exact strings the controller renders (time.Duration.String) when it does.
func TestShutdownTimeoutFlagParses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    time.Duration
		wantErr bool
	}{
		{name: "default when unset", args: nil, want: 30 * time.Second},
		{name: "drainTimeout 600s renders as 10m0s",
			args: []string{"--shutdown-timeout", "10m0s"}, want: 10 * time.Minute},
		{name: "drainTimeout 2s renders as 2s",
			args: []string{"--shutdown-timeout", "2s"}, want: 2 * time.Second},
		{name: "drainTimeout 1500ms renders as 1.5s",
			args: []string{"--shutdown-timeout", "1.5s"}, want: 1500 * time.Millisecond},
		{name: "flag=value form", args: []string{"--shutdown-timeout=5m0s"}, want: 5 * time.Minute},
		{name: "unparsable value fails the process",
			args: []string{"--shutdown-timeout", "soon"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("router-proxy", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			got := registerShutdownTimeout(fs)

			err := fs.Parse(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse %v: want an error, got --shutdown-timeout %s", tc.args, *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if *got != tc.want {
				t.Errorf("parse %v: --shutdown-timeout = %s, want %s", tc.args, *got, tc.want)
			}
		})
	}
}

// TestReadinessRequiredListenerFlagParses drives the real registration with
// the args the controller renders for spec.proxy.readinessRequiredListeners:
// one --readiness-required-listener per port, in spec order.
func TestReadinessRequiredListenerFlagParses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    []uint16
		wantErr bool
	}{
		{name: "none when unset", args: nil, want: nil},
		{name: "one port", args: []string{"--readiness-required-listener", "15008"}, want: []uint16{15008}},
		{name: "repeated", args: []string{
			"--readiness-required-listener", "15008",
			"--readiness-required-listener=15006",
		}, want: []uint16{15008, 15006}},
		{name: "bounds", args: []string{
			"--readiness-required-listener", "1",
			"--readiness-required-listener", "65535",
		}, want: []uint16{1, 65535}},
		{name: "zero fails", args: []string{"--readiness-required-listener", "0"}, wantErr: true},
		{name: "above 65535 fails", args: []string{"--readiness-required-listener", "65536"}, wantErr: true},
		{name: "negative fails", args: []string{"--readiness-required-listener", "-1"}, wantErr: true},
		{name: "comma list fails", args: []string{"--readiness-required-listener", "15008,15006"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("router-proxy", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			got := registerReadinessRequiredListeners(fs)

			err := fs.Parse(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse %v: want an error, got %v", tc.args, *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if !slices.Equal(*got, tc.want) {
				t.Errorf("parse %v: --readiness-required-listener = %v, want %v", tc.args, *got, tc.want)
			}
		})
	}
}
