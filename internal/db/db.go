package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID                 string
	Email              string
	Username           string
	PasswordHash       string
	Role               string
	MustChangePassword bool
}

type Library struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

func EnsureSchema(session *gocql.Session, keyspace string) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.users (
			id uuid PRIMARY KEY,
			email text,
			username text,
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
			kind text,
			created_at timestamp
		)`, keyspace),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS libraries_path_idx ON %s.libraries (path)`, keyspace),
	}
	for _, stmt := range stmts {
		if err := session.Query(stmt).Exec(); err != nil {
			return err
		}
	}
	if err := ensureLibraryKindColumn(session, keyspace); err != nil {
		return err
	}
	if err := ensureUsersUsernameColumn(session, keyspace); err != nil {
		return err
	}
	return nil
}

func ensureLibraryKindColumn(session *gocql.Session, keyspace string) error {
	err := session.Query(fmt.Sprintf(`ALTER TABLE %s.libraries ADD kind text`, keyspace)).Exec()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "already") || strings.Contains(msg, "conflict") {
		return nil
	}
	return err
}

func ensureUsersUsernameColumn(session *gocql.Session, keyspace string) error {
	err := session.Query(fmt.Sprintf(`ALTER TABLE %s.users ADD username text`, keyspace)).Exec()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "already") || strings.Contains(msg, "conflict") {
		return nil
	}
	return err
}

func normalizeUsername(email, username string) string {
	name := strings.TrimSpace(username)
	if name != "" {
		return name
	}
	clean := strings.TrimSpace(email)
	if clean != "" {
		if at := strings.Index(clean, "@"); at > 0 {
			return clean[:at]
		}
		return clean
	}
	return "user"
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
	return CreateUser(ctx, session, keyspace, email, "", password, "admin", true)
}

func CreateUser(ctx context.Context, session *gocql.Session, keyspace, email, username, password, role string, mustChange bool) error {
	id := gocql.TimeUUID()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	normalized := normalizeUsername(email, username)
	return session.Query(fmt.Sprintf(`INSERT INTO %s.users (id,email,username,password_hash,role,must_change,created_at)
		VALUES (?,?,?,?,?,?,?)`, keyspace),
		id, email, normalized, string(hash), role, mustChange, time.Now()).WithContext(ctx).Exec()
}

func Authenticate(ctx context.Context, session *gocql.Session, keyspace, email, password string) (User, error) {
	var u User
	err := session.Query(fmt.Sprintf(`SELECT id,email,username,password_hash,role,must_change FROM %s.users WHERE email=? LIMIT 1`, keyspace), email).
		WithContext(ctx).
		Scan(&u.ID, &u.Email, &u.Username, &u.PasswordHash, &u.Role, &u.MustChangePassword)
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return User{}, errors.New("invalid credentials")
	}
	if u.Username == "" {
		u.Username = normalizeUsername(u.Email, "")
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

func GetUserByID(ctx context.Context, session *gocql.Session, keyspace, userID string) (User, error) {
	var u User
	id, err := gocql.ParseUUID(userID)
	if err != nil {
		return User{}, err
	}
	err = session.Query(fmt.Sprintf(`SELECT id,email,username,role,must_change FROM %s.users WHERE id=?`, keyspace), id).
		WithContext(ctx).
		Scan(&u.ID, &u.Email, &u.Username, &u.Role, &u.MustChangePassword)
	if err != nil {
		return User{}, err
	}
	if u.Username == "" {
		u.Username = normalizeUsername(u.Email, "")
	}
	return u, nil
}

func ListUsers(ctx context.Context, session *gocql.Session, keyspace string) ([]User, error) {
	var users []User
	iter := session.Query(fmt.Sprintf(`SELECT id,email,username,role,must_change FROM %s.users`, keyspace)).
		WithContext(ctx).Iter()
	var u User
	for iter.Scan(&u.ID, &u.Email, &u.Username, &u.Role, &u.MustChangePassword) {
		if u.Username == "" {
			u.Username = normalizeUsername(u.Email, "")
		}
		users = append(users, u)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return users, nil
}

func UpdateUser(ctx context.Context, session *gocql.Session, keyspace, userID, email, username, role string, mustChange bool) error {
	id, err := gocql.ParseUUID(userID)
	if err != nil {
		return err
	}
	normalized := normalizeUsername(email, username)
	return session.Query(fmt.Sprintf(`UPDATE %s.users SET email=?, username=?, role=?, must_change=? WHERE id=?`, keyspace),
		email, normalized, role, mustChange, id).
		WithContext(ctx).
		Exec()
}

func UpdateUserPassword(ctx context.Context, session *gocql.Session, keyspace, userID, password string) error {
	id, err := gocql.ParseUUID(userID)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return session.Query(fmt.Sprintf(`UPDATE %s.users SET password_hash=?, must_change=true WHERE id=?`, keyspace),
		string(hash), id).
		WithContext(ctx).
		Exec()
}

func UpdateProfile(ctx context.Context, session *gocql.Session, keyspace, userID, email, username string) error {
	id, err := gocql.ParseUUID(userID)
	if err != nil {
		return err
	}
	normalized := normalizeUsername(email, username)
	return session.Query(fmt.Sprintf(`UPDATE %s.users SET email=?, username=? WHERE id=?`, keyspace),
		email, normalized, id).
		WithContext(ctx).
		Exec()
}

func ListLibraries(ctx context.Context, session *gocql.Session, keyspace string) ([]Library, error) {
	var libs []Library
	iter := session.Query(fmt.Sprintf(`SELECT id,name,path,kind,created_at FROM %s.libraries`, keyspace)).
		WithContext(ctx).Iter()
	var l Library
	for iter.Scan(&l.ID, &l.Name, &l.Path, &l.Kind, &l.CreatedAt) {
		if l.Kind == "" {
			l.Kind = "movie"
		}
		libs = append(libs, l)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return libs, nil
}

func CreateLibrary(ctx context.Context, session *gocql.Session, keyspace, name, path, kind string) (Library, error) {
	if strings.TrimSpace(kind) == "" {
		kind = "movie"
	}
	var existing Library
	err := session.Query(fmt.Sprintf(`SELECT id,name,path,kind,created_at FROM %s.libraries WHERE path=? LIMIT 1`, keyspace), path).
		WithContext(ctx).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.Kind, &existing.CreatedAt)
	if err == nil && existing.ID != "" {
		if name != "" && existing.Name != name {
			if err := session.Query(fmt.Sprintf(`UPDATE %s.libraries SET name=? WHERE id=?`, keyspace), name, existing.ID).
				WithContext(ctx).Exec(); err != nil {
				return Library{}, err
			}
			existing.Name = name
		}
		if kind != "" && existing.Kind != kind {
			if err := session.Query(fmt.Sprintf(`UPDATE %s.libraries SET kind=? WHERE id=?`, keyspace), kind, existing.ID).
				WithContext(ctx).Exec(); err != nil {
				return Library{}, err
			}
			existing.Kind = kind
		}
		if existing.Kind == "" {
			existing.Kind = "movie"
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return Library{}, err
	}
	id := gocql.TimeUUID()
	now := time.Now()
	if err := session.Query(fmt.Sprintf(`INSERT INTO %s.libraries (id,name,path,kind,created_at) VALUES (?,?,?,?,?)`, keyspace),
		id, name, path, kind, now).WithContext(ctx).Exec(); err != nil {
		return Library{}, err
	}
	return Library{ID: id.String(), Name: name, Path: path, Kind: kind, CreatedAt: now}, nil
}

func DeleteLibrary(ctx context.Context, session *gocql.Session, keyspace, id string) error {
	uid, err := gocql.ParseUUID(id)
	if err != nil {
		return err
	}
	return session.Query(fmt.Sprintf(`DELETE FROM %s.libraries WHERE id=?`, keyspace), uid).WithContext(ctx).Exec()
}
