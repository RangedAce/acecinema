package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type infoPayload struct {
	NodeName string `json:"node_name"`
	Role     string `json:"role"`
	DBHost   string `json:"db_host"`
	DBPort   string `json:"db_port"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	node := env("NODE_NAME", "app")
	role := env("ROLE", "app")
	dbHost := env("DB_HOST", "db")
	dbPort := env("DB_PORT", "5432")
	dbUser := env("DB_USER", "ace")
	dbPassword := env("DB_PASSWORD", "acepass")
	dbName := env("DB_NAME", "acecinema")
	appPort := env("APP_PORT", env("PORT", "8080"))

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, infoPayload{
			NodeName: node,
			Role:     role,
			DBHost:   dbHost,
			DBPort:   dbPort,
		})
	})

	mux.HandleFunc("/db-check", func(w http.ResponseWriter, r *http.Request) {
		connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", dbUser, dbPassword, dbHost, dbPort, dbName)
		db, err := sql.Open("pgx", connStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("open db: %v", err), http.StatusServiceUnavailable)
			return
		}
		defer db.Close()

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.PingContext(ctx); err != nil {
			http.Error(w, fmt.Sprintf("ping db: %v", err), http.StatusServiceUnavailable)
			return
		}
		var out int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&out); err != nil || out != 1 {
			http.Error(w, fmt.Sprintf("select failed: %v", err), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":       true,
			"node":     node,
			"db_host":  dbHost,
			"db_port":  dbPort,
			"selected": out,
		})
	})

	server := &http.Server{
		Addr:              ":" + appPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("placeholder app starting on :%s (node=%s role=%s db=%s:%s)", appPort, node, role, dbHost, dbPort)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server stopped: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
