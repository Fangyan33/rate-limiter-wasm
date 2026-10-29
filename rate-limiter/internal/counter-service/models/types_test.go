package models

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestServiceIdentityValidation(t *testing.T) {
	for _, service := range []string{"ai/inference", "namespace-1/service-2"} {
		require.NoError(t, ValidateService(service, 8000))
	}
	for _, service := range []string{"inference", "inference.ai.svc.cluster.local", "*", "ai/*", "AI/inference", "ai/inference/extra", "/inference", "ai/", " ai/inference", "ai/a.b", "ai/-bad", "ai/" + strings.Repeat("x", 64)} {
		require.Error(t, ValidateService(service, 8000), service)
	}
	for _, port := range []int{-1, 0, 65536} {
		require.Error(t, ValidateService("ai/inference", port))
	}
	require.NoError(t, ValidateService("ai/inference", 65535))
}

func TestParseUpstreamCluster(t *testing.T) {
	for _, subset := range []string{"", "v1", "v2"} {
		for _, domain := range []string{"cluster.local", "corp.internal"} {
			service, port, ok := ParseUpstreamCluster("outbound|8000|"+subset+"|inference.ai.svc."+domain, domain)
			require.True(t, ok)
			require.Equal(t, "ai/inference", service)
			require.Equal(t, 8000, port)
		}
	}
	for _, cluster := range []string{"", "PassthroughCluster", "BlackHoleCluster", "inbound|8000||inference.ai.svc.cluster.local", "outbound|0||inference.ai.svc.cluster.local", "outbound|65536||inference.ai.svc.cluster.local", "outbound|080||inference.ai.svc.cluster.local", "outbound|8000||inference.ai.svc.other", "outbound|8000||example.com", "outbound|8000||inference.ai.svc.cluster.local.evil", "outbound|8000||inference.ai.svc.cluster.local|extra", strings.Repeat("x", 1025)} {
		_, _, ok := ParseUpstreamCluster(cluster, "cluster.local")
		require.False(t, ok, cluster)
	}
	_, _, ok := ParseUpstreamCluster("outbound|8000||inference.ai.svc.cluster.local", "")
	require.False(t, ok)
}

func BenchmarkParseUpstreamCluster(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, ok := ParseUpstreamCluster("outbound|8000|v1|inference.ai.svc.cluster.local", "cluster.local")
		if !ok {
			b.Fatal("not recognized")
		}
	}
}
