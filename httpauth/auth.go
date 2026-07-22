// Package httpauth protects the agent's admin API with the same JWT scheme
// as the Go backend (backend/internal/middleware/auth.go): same JWT_SECRET,
// same "roles" claim. A token obtained by logging into the admin app works
// unmodified here, with no separate login flow for the agent service.
package httpauth

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// RequireAdmin returns middleware that rejects requests without a valid JWT
// carrying the "admin" role.
func RequireAdmin(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				writeError(w, http.StatusForbidden, "JWT_SECRET non configuré")
				return
			}

			tokenString := extractBearer(r.Header.Get("Authorization"))
			if tokenString == "" {
				writeError(w, http.StatusUnauthorized, "token manquant")
				return
			}

			claims, err := parseToken(tokenString, secret)
			if err != nil {
				writeError(w, http.StatusUnauthorized, err.Error())
				return
			}

			if !hasRole(claims, "admin") {
				writeError(w, http.StatusForbidden, "rôle insuffisant")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func parseToken(tokenString, secret string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("token invalide")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("claims invalides")
	}
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil, fmt.Errorf("token expiré")
		}
	}
	return claims, nil
}

func hasRole(claims jwt.MapClaims, role string) bool {
	rawRoles, ok := claims["roles"].([]interface{})
	if !ok {
		return false
	}
	for _, r := range rawRoles {
		if s, ok := r.(string); ok && s == role {
			return true
		}
	}
	return false
}

func extractBearer(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, message)
}
