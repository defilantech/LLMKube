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
