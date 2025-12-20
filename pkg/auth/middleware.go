package auth

import (
	"net/http"
)

// TokenMiddleware enforces a fixed token in header X-API-Token.
func TokenMiddleware(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" {
				http.Error(w, "api token not configured", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-API-Token") != expected {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
