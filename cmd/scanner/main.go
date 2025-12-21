package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"acecinema/internal/db"
	"acecinema/internal/media"
)

func main() {
	hosts := strings.Split(os.Getenv("SCYLLA_HOSTS"), ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	keyspace := env("SCYLLA_KEYSPACE", "acecinema")
	cluster := gocql.NewCluster(hosts...)
	cluster.Port = envInt("SCYLLA_PORT", 9042)
	cluster.Timeout = 5 * time.Second
	cluster.Consistency = gocql.Quorum

	var session *gocql.Session

	// first connect without keyspace to ensure it exists
	for i := 0; i < 10; i++ {
		s, err := cluster.CreateSession()
		if err == nil {
			session = s
			break
		}
		log.Printf("scylla connect retry %d/10: %v", i+1, err)
		time.Sleep(3 * time.Second)
	}
	if session == nil {
		log.Fatalf("scylla connect: giving up")
	}
	if err := db.EnsureKeyspace(session, keyspace, 3); err != nil {
		session.Close()
		log.Fatalf("ensure keyspace: %v", err)
	}
	session.Close()

	// reconnect with keyspace
	cluster.Keyspace = keyspace
	for i := 0; i < 10; i++ {
		s, err := cluster.CreateSession()
		if err == nil {
			session = s
			break
		}
		log.Printf("scylla connect (with keyspace) retry %d/10: %v", i+1, err)
		time.Sleep(3 * time.Second)
	}
	if session == nil {
		log.Fatalf("scylla connect with keyspace: giving up")
	}
	defer session.Close()

	mediaRoot := env("MEDIA_ROOT", "/mnt/media")
	svc := media.NewService(session, keyspace, mediaRoot)
	log.Printf("scanner starting (media_root=%s)", mediaRoot)
	if err := svc.Scan(context.Background()); err != nil {
		log.Fatalf("scan error: %v", err)
	}
	log.Println("scanner completed")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var out int
		if _, err := fmt.Sscanf(v, "%d", &out); err == nil {
			return out
		}
	}
	return def
}
