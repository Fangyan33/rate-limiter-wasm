package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/config"
)

func TestDomainConfiguration(t *testing.T) {
	for _, domains := range []string{"", "domains: []\n", "domains: null\n"} {
		cfg, err := config.Parse([]byte(domains + enabled))
		require.NoError(t, err)
		require.Empty(t, cfg.Domains)
	}
	cfg, err := config.Parse([]byte("domains: [' API.Example.COM.:443 ', '*.Example.ORG.:80']\n" + enabled))
	require.NoError(t, err)
	require.Equal(t, config.DomainPatterns{"api.example.com", "*.example.org"}, cfg.Domains)
	for _, domains := range []string{
		"api.example.com", "42", "{}", "[42]", "[true]", "[null]", "['']", "['  ']",
		"['*']", "['*.']", "['*.*.example.com']", "['api-*.example.com']", "['example.*']",
		"['https://example.com']", "['example.com/path']", "['example.com:bad']", "['.example.com']",
	} {
		t.Run(domains, func(t *testing.T) {
			_, err := config.Parse([]byte("domains: " + domains + "\n" + enabled))
			require.Error(t, err)
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" API.Example.COM.:443 ", "api.example.com"}, {"example.com", "example.com"},
		{"127.0.0.1:8080", "127.0.0.1"}, {"[2001:DB8::1]:8080", "2001:db8::1"}, {"::1", "::1"},
		{"", ""}, {" ", ""}, {"example.com:", ""}, {"example.com:bad", ""},
		{"example.com:65536", ""}, {"example.com:-1", ""}, {"https://example.com", ""},
		{"example.com..", ""}, {"::1]", ""}, {"[::1", ""},
		{"user@example.com", ""}, {"a..example.com", ""}, {"bad host", ""}, {"*.example.com", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			require.Equal(t, tc.want, config.NormalizeHost(tc.input))
		})
	}
}
