package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lim-worker-go/pkg/executor"
)

var (
	port         = getEnv("PORT", "3500")
	workerSecret = getEnv("WORKER_SECRET", "lim_sg_worker_secret_2026")
	startTime    = time.Now()
)

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists && value != "" {
		return value
	}
	return fallback
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := r.Header.Get("X-Worker-Secret")
		if secret == "" || secret != workerSecret {
			log.Printf("[GoWorker] 🚫 Unauthorized request from %s\n", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Unauthorized",
			})
			return
		}
		next(w, r)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"worker":    "golang",
		"uptime":    time.Since(startTime).Seconds(),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func executeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload executor.Payload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid JSON body: %v", err),
		})
		return
	}

	if payload.SessionKey == "" || payload.FAToken == "" || len(payload.Players) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Missing required fields: session_key, fa_token, players[]",
		})
		return
	}

	log.Printf("[GoWorker] 📥 RECEIVED: %d player(s) to process\n", len(payload.Players))
	result := executor.Execute(payload)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/execute", authMiddleware(executeHandler))

	server := &http.Server{
		Addr:         ":" + port,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		log.Println("════════════════════════════════════════════════════════════")
		log.Printf("  🇸🇬 Go LIM Worker Server running on port %s\n", port)
		log.Printf("  Secret: %s...\n", workerSecret[:8])
		log.Println("════════════════════════════════════════════════════════════")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v\n", err)
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("\n🛑 Go Worker shutting down...")
}
