package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

type infoPayload struct {
	NodeName     string   `json:"node_name"`
	Role         string   `json:"role"`
	ScyllaHosts  []string `json:"scylla_hosts"`
	ScyllaPort   string   `json:"scylla_port"`
	ScyllaKS     string   `json:"scylla_keyspace"`
	ScyllaUser   string   `json:"scylla_user,omitempty"`
	HasDBCheck   bool     `json:"db_check_enabled"`
	LastDBStatus string   `json:"last_db_status,omitempty"`
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
	hostsEnv := env("SCYLLA_HOSTS", "scylla1,scylla2,scylla3")
	scyllaHosts := splitAndTrim(hostsEnv)
	scyllaPort := env("SCYLLA_PORT", "9042")
	scyllaKS := env("SCYLLA_KEYSPACE", "acecinema")
	scyllaUser := os.Getenv("SCYLLA_USER")
	scyllaPass := os.Getenv("SCYLLA_PASSWORD")
	appPort := env("APP_PORT", env("PORT", "8080"))

	cluster := gocql.NewCluster(scyllaHosts...)
	cluster.Port = mustAtoi(scyllaPort, 9042)
	if scyllaKS != "" {
		cluster.Keyspace = scyllaKS
	} else {
		cluster.Keyspace = "system"
	}
	cluster.Timeout = 3 * time.Second
	cluster.Consistency = gocql.Quorum
	if scyllaUser != "" {
		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: scyllaUser,
			Password: scyllaPass,
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, infoPayload{
			NodeName:     node,
			Role:         role,
			ScyllaHosts:  scyllaHosts,
			ScyllaPort:   scyllaPort,
			ScyllaKS:     scyllaKS,
			ScyllaUser:   scyllaUser,
			HasDBCheck:   true,
			LastDBStatus: "unknown",
		})
	})

	mux.HandleFunc("/db-check", func(w http.ResponseWriter, r *http.Request) {
		clusterName, err := pingScylla(r, cluster, scyllaKS)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":           true,
			"node":         node,
			"role":         role,
			"scylla_hosts": scyllaHosts,
			"scylla_port":  scyllaPort,
			"cluster":      clusterName,
		})
	})

	server := &http.Server{
		Addr:              ":" + appPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("placeholder app starting on :%s (node=%s role=%s scylla=%s:%s)", appPort, node, role, hostsEnv, scyllaPort)
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

func splitAndTrim(in string) []string {
	parts := strings.Split(in, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustAtoi(v string, fallback int) int {
	if v == "" {
		return fallback
	}
	var out int
	_, err := fmt.Sscanf(v, "%d", &out)
	if err != nil {
		return fallback
	}
	return out
}

func pingScylla(r *http.Request, cluster *gocql.ClusterConfig, keyspace string) (string, error) {
	// try with configured keyspace
	session, err := cluster.CreateSession()
	if err != nil && keyspace != "" {
		// fallback without keyspace to allow creating it
		tmp := *cluster
		tmp.Keyspace = "system"
		session, err = tmp.CreateSession()
		if err != nil {
			return "", err
		}
		defer session.Close()
		if err := session.Query(fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 3}", keyspace)).WithContext(r.Context()).Exec(); err != nil {
			return "", err
		}
		// retry with target keyspace
		tmp.Keyspace = keyspace
		session, err = tmp.CreateSession()
		if err != nil {
			return "", err
		}
	}
	if session == nil {
		return "", fmt.Errorf("no session")
	}
	defer session.Close()

	var clusterName string
	if err := session.Query("SELECT cluster_name FROM system.local").WithContext(r.Context()).Scan(&clusterName); err != nil {
		return "", err
	}
	return clusterName, nil
}
