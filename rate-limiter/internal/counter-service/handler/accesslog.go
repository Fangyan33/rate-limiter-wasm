package handler

import (
	"encoding/hex"
	"hash/fnv"
	"log"
	"net/http"
	"time"
)

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func newLoggingResponseWriter(w http.ResponseWriter) *loggingResponseWriter {
	// Default status code if handler never calls WriteHeader.
	return &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func shortHash12(s string) string {
	if s == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	// 8 bytes => 16 hex chars; take first 12 for brevity.
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// maskToken returns a non-reversible identifier that does NOT contain any raw
// token characters (so it is safe against substring leaks).
func maskToken(s string) string {
	if s == "" {
		return ""
	}
	// Keep it simple: only log the length.
	return "len" + itoa(len(s))
}

// tiny integer formatting without pulling in fmt.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}

type accessLogMeta struct {
	apiKey   string
	leaseID  string
	allowed  *bool
	released *bool
}

func logAccess(r *http.Request, w *loggingResponseWriter, startedAt time.Time, meta accessLogMeta) {
	latency := time.Since(startedAt)

	apiKeyHash := shortHash12(meta.apiKey)
	apiKeyMask := maskToken(meta.apiKey)
	leaseHash := shortHash12(meta.leaseID)
	leaseMask := maskToken(meta.leaseID)

	allowed := ""
	if meta.allowed != nil {
		if *meta.allowed {
			allowed = "true"
		} else {
			allowed = "false"
		}
	}

	released := ""
	if meta.released != nil {
		if *meta.released {
			released = "true"
		} else {
			released = "false"
		}
	}

	// Keep this stable and grep-friendly.
	log.Printf(
		"access method=%s path=%s status=%d bytes=%d latency_ms=%d remote=%s ua=%q api_key_hash=%s api_key_mask=%s lease_id_hash=%s lease_id_mask=%s allowed=%s released=%s",
		r.Method,
		r.URL.Path,
		w.status,
		w.bytes,
		latency.Milliseconds(),
		r.RemoteAddr,
		r.UserAgent(),
		apiKeyHash,
		apiKeyMask,
		leaseHash,
		leaseMask,
		allowed,
		released,
	)
}
