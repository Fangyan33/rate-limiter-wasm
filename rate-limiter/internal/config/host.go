package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DomainPatterns rejects YAML's implicit conversion of numbers and booleans to strings.
type DomainPatterns []string

func (d *DomainPatterns) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("domains must be a list of strings")
	}
	patterns := make(DomainPatterns, 0, len(node.Content))
	for _, entry := range node.Content {
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
			return fmt.Errorf("domains must contain only strings")
		}
		patterns = append(patterns, entry.Value)
	}
	*d = patterns
	return nil
}

func normalizeDomainPattern(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	wildcard := strings.HasPrefix(pattern, "*.")
	if wildcard {
		pattern = strings.TrimPrefix(pattern, "*.")
	}
	host := NormalizeHost(pattern)
	if host == "" {
		return "", fmt.Errorf("expected a host or *.host pattern")
	}
	if wildcard {
		return "*." + host, nil
	}
	return host, nil
}

// NormalizeHost removes an optional port and DNS trailing dot. Invalid authorities
// return an empty string so the caller can bypass rate limiting.
func NormalizeHost(authority string) string {
	host := strings.ToLower(strings.TrimSpace(authority))
	if strings.Contains(host, ":") {
		if parsed, port, err := net.SplitHostPort(host); err == nil {
			if port == "" {
				return ""
			}
			for _, c := range port {
				if c < '0' || c > '9' {
					return ""
				}
			}
			if n, err := strconv.Atoi(port); err != nil || n > 65535 {
				return ""
			}
			host = parsed
		} else {
			// Accept a bare IPv6 address as well as [IPv6]:port.
			if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
				host = host[1 : len(host)-1]
			}
			if net.ParseIP(host) == nil {
				return ""
			}
		}
		if strings.Contains(host, ":") {
			if net.ParseIP(host) == nil {
				return ""
			}
			return host
		}
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return ""
	}
	for _, c := range host {
		if c != '.' && c != '-' && c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return host
}
