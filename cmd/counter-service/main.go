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
	port := getEnv("PORT", "8080")

	client, err := redis.NewClient(redisAddr, redisPassword, 0, 10, 3)
	if err != nil {
		log.Fatalf("failed to create redis client: %v", err)
	}
	defer client.Close()

	if err := client.Ping(context.Background()); err != nil {
		log.Fatalf("failed to ping redis: %v", err)
	}
	log.Printf("connected to redis at %s", redisAddr)

	h := handler.NewHandler(client)

	mux := http.NewServeMux()
	mux.HandleFunc("/acquire", h.Acquire)
	mux.HandleFunc("/release", h.Release)
	mux.HandleFunc("/health", h.Health)

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
