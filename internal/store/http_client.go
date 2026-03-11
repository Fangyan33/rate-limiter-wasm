package store

type httpCounterServiceClient struct {
	cluster     string
	timeoutMS   int
	acquirePath string
	releasePath string
	leaseTTLMS  int
}
