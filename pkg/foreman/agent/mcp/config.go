package mcp

import "time"

type ServerConfig struct {
	Name         string
	URL          string
	Headers      map[string]string
	AllowedTools []string
}

type Options struct {
	CallTimeout    time.Duration
	MaxResultBytes int
}

func (o Options) withDefaults() Options {
	if o.CallTimeout <= 0 {
		o.CallTimeout = 30 * time.Second
	}
	if o.MaxResultBytes <= 0 {
		o.MaxResultBytes = 32768
	}
	return o
}

func allowed(tool string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == "*" || a == tool {
			return true
		}
	}
	return false
}

// unmatchedAllowlistEntries returns the explicit allowlist entries that match
// none of the discovered tool names. An empty allowlist, or one containing
// "*", means allow-all, so there is nothing to flag (returns nil). This
// surfaces a stale or mistyped tool name that the allowlist would otherwise
// drop silently (#1183: context7's tool was renamed get-library-docs ->
// query-docs, and the crippled server went unnoticed for weeks).
func unmatchedAllowlistEntries(allow, discovered []string) []string {
	if len(allow) == 0 {
		return nil
	}
	for _, a := range allow {
		if a == "*" {
			return nil
		}
	}
	have := make(map[string]struct{}, len(discovered))
	for _, d := range discovered {
		have[d] = struct{}{}
	}
	var unmatched []string
	for _, a := range allow {
		if _, ok := have[a]; !ok {
			unmatched = append(unmatched, a)
		}
	}
	return unmatched
}
