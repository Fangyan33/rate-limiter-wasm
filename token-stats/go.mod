module rate-limiter-wasm/token-stats

go 1.22

require (
	github.com/stretchr/testify v1.9.0
	github.com/tetratelabs/proxy-wasm-go-sdk v0.24.0
	gopkg.in/yaml.v3 v3.0.1
	rate-limiter-wasm/shared v0.0.0
)

replace rate-limiter-wasm/shared => ../shared
