package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rate-limiter-wasm/internal/counter-service/handler"
	"rate-limiter-wasm/internal/counter-service/redis"
)

func main() {
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	redisPassword := getEnv("REDIS_PASSWORD", "")
	keyPrefix := getEnv("REDIS_KEY_PREFIX", "rl:")
	port := getEnv("PORT", "8080")

	client, err := redis.NewClient(redis.Config{
		Addr:      redisAddr,
		Password:  redisPassword,
		DB:        0,
		KeyPrefix: keyPrefix,
	})
	if err != nil {
		log.Fatalf("failed to create redis client: %v", err)
	}
	defer client.Close()

	if err := client.Ping(context.Background()); err != nil {
		log.Fatalf("failed to ping redis: %v", err)
	}
	log.Printf("connected to redis at %s", redisAddr)

	acquireHandler := handler.NewAcquireHandler(client)
	releaseHandler := handler.NewReleaseHandler(client)
	configHandler := handler.NewConfigHandler(client)

	mux := http.NewServeMux()
	mux.Handle("/acquire", acquireHandler)
	mux.Handle("/release", releaseHandler)
	mux.Handle("/config", configHandler)
	mux.Handle("/configs", configHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("starting server on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server shutdown error: %v", err)
	}
	log.Println("server stopped")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
