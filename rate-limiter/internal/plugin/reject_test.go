package plugin

import (
	"testing"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestHTTPContextRejectReturnsContinueWhenRootNil(t *testing.T) {
	h := &httpContext{}

	if got := h.reject(); got != types.ActionContinue {
		t.Fatalf("reject() action = %v, want %v", got, types.ActionContinue)
	}
}

func TestHTTPContextRejectReturnsContinueWhenSendResponseFails(t *testing.T) {
	h := &httpContext{root: &rootContext{}}

	if got := h.reject(); got != types.ActionContinue {
		t.Fatalf("reject() action = %v, want %v", got, types.ActionContinue)
	}
}
