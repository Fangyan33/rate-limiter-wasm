package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcquireInternalFailureIsNotAPolicyDenial(t *testing.T) {
	h, _, mr := setupTestHandler(t)
	mr.SetError("ERR injected script failure")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/acquire",
		strings.NewReader(`{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","ttl_ms":30000}`)))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.JSONEq(t, `{"allowed":false,"reason":"internal_error","message":"eval acquire script: ERR injected script failure"}`, w.Body.String())
}
