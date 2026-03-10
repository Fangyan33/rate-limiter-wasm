package matcher

import (
	"fmt"
	"strings"
)

type DomainMatcher struct {
	patterns []string
}

func NewDomainMatcher(patterns []string) (*DomainMatcher, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("domain patterns must not be empty")
	}

	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("domain pattern must not be empty")
		}
		if strings.HasPrefix(pattern, "*.") && len(pattern) <= 2 {
			return nil, fmt.Errorf("wildcard domain pattern must include a suffix")
		}
		normalized = append(normalized, strings.ToLower(pattern))
	}

	return &DomainMatcher{patterns: normalized}, nil
}

func (m *DomainMatcher) Match(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}

	for _, pattern := range m.patterns {
		if pattern == host {
			return true
		}

		if !strings.HasPrefix(pattern, "*.") {
			continue
		}

		suffix := strings.TrimPrefix(pattern, "*")
		base := strings.TrimPrefix(pattern, "*.")
		if host == base {
			continue
		}
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}

	return false
}
