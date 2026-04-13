package main

import (
	"rate-limiter-wasm/token-stats/internal/plugin"

	proxywasm "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
)

func main() {
	proxywasm.SetVMContext(plugin.NewVMContext())
}
