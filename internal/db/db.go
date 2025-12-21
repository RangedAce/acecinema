package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocql/gocql"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID                 string
	Email              string
	PasswordHash       string
	Role               string
	MustChangePassword bool
}

func EnsureSchema(session *gocql.Session, keyspace string) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.users (
			id uuid PRIMARY KEY,
			email text,
			password_hash text,
			role text,
			must_change boolean,
			created_at timestamp
		)`, keyspace),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS users_email_idx ON %s.users (email)`, keyspace),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.media_items (
			id uuid PRIMARY KEY,
			type text,
			title text,
			year int,
			season int,
			episode int,
			show_id uuid,
			metadata map<text,text>,
			poster_url text,
			backdrop_url text,
			created_at timestamp
		)`, keyspace),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.media_assets (
			id uuid,
			media_id uuid,
			path text,
			size bigint,
			format text,
			PRIMARY KEY (media_id, id)
		)`, keyspace),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.media_paths (
			path text PRIMARY KEY,
			media_id uuid
		)`, keyspace),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.play_state (
			user_id uuid,
			media_id uuid,
			position_ms bigint,
			updated_at timestamp,
			PRIMARY KEY (user_id, media_id)
		)`, keyspace),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.libraries (
			id uuid PRIMARY KEY,
			name text,
			path text,
			created_at timestamp
		)`, keyspace),
	}
	for _, stmt := range stmts {
		if err := session.Query(stmt).Exec(); err != nil {
			return err
		}
	}
	return nil
}

func EnsureKeyspace(session *gocql.Session, keyspace string, replicationFactor int) error {
	if replicationFactor <= 0 {
		replicationFactor = 3
	}
	stmt := fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'SimpleStrategy', 'replication_factor': %d}", keyspace, replicationFactor)
	return session.Query(stmt).Exec()
}

func EnsureAdmin(ctx context.Context, session *gocql.Session, keyspace, email, password string) error {
	var existing string
	err := session.Query(fmt.Sprintf("SELECT id FROM %s.users WHERE email=? LIMIT 1", keyspace), email).WithContext(ctx).Scan(&existing)
	if err == nil && existing != "" {
		return nil
	}
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return err
	}
	return CreateUser(ctx, session, keyspace, email, password, "admin", true)
}

func CreateUser(ctx context.Context, session *gocql.Session, keyspace, email, password, role string, mustChange bool) error {
	id := gocql.TimeUUID()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return session.Query(fmt.Sprintf(`INSERT INTO %s.users (id,email,password_hash,role,must_change,created_at)
		VALUES (?,?,?,?,?,?)`, keyspace),
		id, email, string(hash), role, mustChange, time.Now()).WithContext(ctx).Exec()
}

func Authenticate(ctx context.Context, session *gocql.Session, keyspace, email, password string) (User, error) {
	var u User
	err := session.Query(fmt.Sprintf(`SELECT id,email,password_hash,role,must_change FROM %s.users WHERE email=? LIMIT 1`, keyspace), email).
		WithContext(ctx).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.MustChangePassword)
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return User{}, errors.New("invalid credentials")
	}
	return u, nil
}

func ChangePassword(ctx context.Context, session *gocql.Session, keyspace, userID, old, new string) error {
	var hash string
	err := session.Query(fmt.Sprintf(`SELECT password_hash FROM %s.users WHERE id=?`, keyspace), userID).
		WithContext(ctx).
		Scan(&hash)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(old)) != nil {
		return errors.New("invalid password")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(new), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return session.Query(fmt.Sprintf(`UPDATE %s.users SET password_hash=?, must_change=false WHERE id=?`, keyspace), string(newHash), userID).
		WithContext(ctx).
		Exec()
}
