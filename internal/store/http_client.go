package store

// httpCounterServiceClient is a placeholder struct for counter_service configuration.
//
// IMPORTANT: This struct is NOT used for actual HTTP calls in counter_service mode.
// The real HTTP callouts are performed directly in internal/plugin/root.go using
// proxywasm.DispatchHttpCall() for async operation.
//
// This struct only exists to satisfy the client construction in NewClient(),
// which validates configuration but returns a non-functional placeholder.
type httpCounterServiceClient struct {
	cluster     string
	timeoutMS   int
	acquirePath string
	releasePath string
	leaseTTLMS  int
}
