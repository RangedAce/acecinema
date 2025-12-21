package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID           string
	Role             string
	MustChangePasswd bool
}

type jwtClaims struct {
	UserID           string `json:"uid"`
	Role             string `json:"role"`
	MustChangePasswd bool   `json:"must_change"`
	jwt.RegisteredClaims
}

type Service struct {
	secret []byte
}

func NewService(secret string) *Service {
	return &Service{secret: []byte(secret)}
}

func (s *Service) GenerateTokens(userID, role string, mustChange bool) (string, string, error) {
	now := time.Now()
	accessClaims := jwtClaims{
		UserID:           userID,
		Role:             role,
		MustChangePasswd: mustChange,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	refreshClaims := jwtClaims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(7 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString(s.secret)
	if err != nil {
		return "", "", err
	}
	refresh, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString(s.secret)
	if err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

func (s *Service) parseToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(token *jwt.Token) (interface{}, error) {
		return s.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	c, ok := token.Claims.(*jwtClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	return &Claims{UserID: c.UserID, Role: c.Role, MustChangePasswd: c.MustChangePasswd}, nil
}

func (s *Service) ParseToken(tokenStr string) (*Claims, error) {
	return s.parseToken(tokenStr)
}

func (s *Service) Refresh(refreshToken string) (string, string, error) {
	claims, err := s.parseToken(refreshToken)
	if err != nil {
		return "", "", err
	}
	return s.GenerateTokens(claims.UserID, claims.Role, claims.MustChangePasswd)
}

type ctxKey string

const claimsKey ctxKey = "claims"

func ClaimsFromContext(ctx context.Context) *Claims {
	val, ok := ctx.Value(claimsKey).(*Claims)
	if !ok {
		return nil
	}
	return val
}

func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		parts := strings.SplitN(authz, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			http.Error(w, "invalid auth header", http.StatusUnauthorized)
			return
		}
		claims, err := s.parseToken(parts[1])
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := ClaimsFromContext(r.Context())
			if claims == nil || claims.Role != role {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}
