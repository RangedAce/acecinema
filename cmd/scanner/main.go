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
	cluster.Consistency = parseConsistency(env("SCYLLA_CONSISTENCY", "QUORUM"))

	var session *gocql.Session

	// first connect without keyspace to ensure it exists
	for i := 0; i < 20; i++ {
		s, err := cluster.CreateSession()
		if err == nil {
			session = s
			break
		}
		log.Printf("scylla connect retry %d/20: %v", i+1, err)
		time.Sleep(5 * time.Second)
	}
	if session == nil {
		log.Fatalf("scylla connect: giving up")
	}
	if err := db.EnsureKeyspace(session, keyspace, envInt("SCYLLA_RF", 3)); err != nil {
		session.Close()
		log.Fatalf("ensure keyspace: %v", err)
	}
	session.Close()

	// reconnect with keyspace
	cluster.Keyspace = keyspace
	for i := 0; i < 20; i++ {
		s, err := cluster.CreateSession()
		if err == nil {
			session = s
			break
		}
		log.Printf("scylla connect (with keyspace) retry %d/20: %v", i+1, err)
		time.Sleep(5 * time.Second)
	}
	if session == nil {
		log.Fatalf("scylla connect with keyspace: giving up")
	}
	defer session.Close()

	mediaRoot := env("MEDIA_ROOT", "")
	svc := media.NewService(session, keyspace, mediaRoot)
	interval := envDuration("SCAN_INTERVAL", 10*time.Minute)

	log.Printf("scanner starting (media_root=%s, interval=%s)", mediaRoot, interval)
	for {
		roots, err := loadLibraryRoots(context.Background(), session, keyspace, mediaRoot)
		if err != nil {
			log.Printf("load libraries: %v", err)
			time.Sleep(interval)
			continue
		}
		if len(roots) == 0 {
			log.Printf("no library roots configured")
			time.Sleep(interval)
			continue
		}
		added, err := svc.ScanRoots(context.Background(), roots)
		if err != nil {
			log.Printf("scan error: %v", err)
		} else {
			log.Printf("scanner completed: %d new items", added)
		}
		time.Sleep(interval)
	}
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

func parseConsistency(c string) gocql.Consistency {
	switch strings.ToUpper(c) {
	case "ONE":
		return gocql.One
	case "LOCAL_ONE":
		return gocql.LocalOne
	case "LOCAL_QUORUM":
		return gocql.LocalQuorum
	case "ALL":
		return gocql.All
	default:
		return gocql.Quorum
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func loadLibraryRoots(ctx context.Context, session *gocql.Session, keyspace, fallback string) ([]string, error) {
	libs, err := db.ListLibraries(ctx, session, keyspace)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(libs))
	for _, lib := range libs {
		if strings.TrimSpace(lib.Path) != "" {
			roots = append(roots, lib.Path)
		}
	}
	if len(roots) == 0 && fallback != "" {
		roots = append(roots, fallback)
	}
	return roots, nil
}
