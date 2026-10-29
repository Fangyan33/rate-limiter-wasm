package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

func TestAcquireHandler_RedisFailureReturns503OverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failure       string
		parentTimeout time.Duration
		maxElapsed    time.Duration
	}{
		{name: "connection refused", failure: "closed", maxElapsed: 800 * time.Millisecond},
		{name: "redis accepts but never replies", failure: "no_reply", maxElapsed: 800 * time.Millisecond},
		{name: "connection pool exhausted", failure: "pool", maxElapsed: 800 * time.Millisecond},
		{name: "earlier caller deadline", failure: "no_reply", parentTimeout: 100 * time.Millisecond, maxElapsed: 400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client, err := redis.NewClient(redis.Config{Addr: mr.Addr(), PoolSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			switch tc.failure {
			case "closed":
				mr.Close()
			case "no_reply":
				// 吞掉 EVAL 且不写任何响应，验证实际 socket 读超时。
				mr.Server().SetPreHook(func(_ *server.Peer, command string, _ ...string) bool {
					return command == "EVAL"
				})
			case "pool":
				conn := client.Rdb().Conn()
				if err := conn.Ping(context.Background()).Err(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
			}

			handler := NewAcquireHandler(client)
			httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.parentTimeout > 0 {
					ctx, cancel := context.WithTimeout(r.Context(), tc.parentTimeout)
					defer cancel()
					r = r.WithContext(ctx)
				}
				handler.ServeHTTP(w, r)
			}))
			httpServer.Config.WriteTimeout = 5 * time.Second
			httpServer.Start()
			t.Cleanup(httpServer.Close)
			httpClient := httpServer.Client()
			httpClient.Timeout = 2 * time.Second

			started := time.Now()
			response, err := httpClient.Post(httpServer.URL+"/acquire", "application/json",
				bytes.NewBufferString(`{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","api_key":"key","ttl_ms":30000}`))
			if err != nil {
				t.Fatalf("/acquire 未返回完整 HTTP 响应: %v", err)
			}
			defer response.Body.Close()
			var body struct {
				Allowed *bool  `json:"allowed"`
				Reason  string `json:"reason"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("响应体不完整或不是 JSON: %v", err)
			}
			elapsed := time.Since(started)
			if response.StatusCode != http.StatusServiceUnavailable || body.Allowed == nil || *body.Allowed || body.Reason != "redis_unavailable" {
				t.Fatalf("期望 HTTP 503 + allowed=false + redis_unavailable，实际 status=%d body=%+v", response.StatusCode, body)
			}
			if elapsed >= tc.maxElapsed {
				t.Errorf("耗时 %s，必须小于 %s", elapsed, tc.maxElapsed)
			}
			t.Logf("HTTP 503 完整返回，耗时 %s", elapsed)
		})
	}
}
