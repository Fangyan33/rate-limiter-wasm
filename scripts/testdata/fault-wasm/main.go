// An intentionally faulty module used only by the isolated fail-open E2E.
package main

import (
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func main() { proxywasm.SetVMContext(&vm{}) }

type vm struct{ types.DefaultVMContext }

func (*vm) NewPluginContext(uint32) types.PluginContext { return &root{} }

type root struct {
	types.DefaultPluginContext
	mode string
}

func (r *root) OnPluginStart(int) types.OnPluginStartStatus {
	data, _ := proxywasm.GetPluginConfiguration()
	r.mode = string(data)
	proxywasm.LogWarn("failure fixture started: " + r.mode)
	if r.mode == "fail_config" {
		return types.OnPluginStartStatusFailed
	}
	return types.OnPluginStartStatusOK
}

func (r *root) NewHttpContext(uint32) types.HttpContext { return &stream{mode: r.mode} }

type stream struct {
	types.DefaultHttpContext
	mode string
}

func (h *stream) OnHttpRequestHeaders(int, bool) types.Action {
	if h.mode == "trap_request" {
		proxywasm.LogWarn("injecting trap_request")
		panic("injected request trap")
	}
	return types.ActionContinue
}

func (h *stream) OnHttpResponseBody(int, bool) types.Action {
	if h.mode == "trap_response" {
		proxywasm.LogWarn("injecting trap_response")
		panic("injected response trap")
	}
	return types.ActionContinue
}
