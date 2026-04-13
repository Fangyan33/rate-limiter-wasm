module rate-limiter-wasm/rate-limiter

go 1.22

require (
	github.com/alicebob/miniredis/v2 v2.33.0
	github.com/google/uuid v1.6.0
	github.com/redis/go-redis/v9 v9.18.0
	github.com/stretchr/testify v1.9.0
	github.com/tetratelabs/proxy-wasm-go-sdk v0.24.0
	gopkg.in/yaml.v3 v3.0.1
	rate-limiter-wasm/shared v0.0.0
)

replace rate-limiter-wasm/shared => ../shared
