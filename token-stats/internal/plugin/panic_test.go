package plugin

import "testing"

func TestMetricWritePanicDisablesStatisticsWithoutEscapingCallback(t *testing.T) {
	root := &rootContext{}
	if err := root.LoadConfiguration([]byte("domains: [api.example.com]\ntoken_statistics:\n  enabled: true")); err != nil {
		t.Fatal(err)
	}
	root.metricKeys = nil // Simulate an internal metric-store failure.
	h := &httpContext{root: root, tokenStatsEnabled: true, domain: "api.example.com", uid: "test", promptTokens: 1}
	h.OnHttpStreamDone()
	if h.tokenStatsEnabled {
		t.Fatal("metric failure must disable statistics for this stream")
	}
}
