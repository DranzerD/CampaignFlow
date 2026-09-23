package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"ad-service/internal/db"
)

// tokenTTL is how long an issued JWT stays valid.
const tokenTTL = 24 * time.Hour

type ctxKey string

const advertiserIDKey ctxKey = "advertiser_id"

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login exchanges advertiser credentials for a JWT.
func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	adv, err := a.Store.AdvertiserByEmail(r.Context(), req.Email)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		log.Printf("login lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(adv.PasswordHash), []byte(req.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"advertiser_id": adv.ID,
		"email":         adv.Email,
		"exp":           time.Now().Add(tokenTTL).Unix(),
	})
	signed, err := token.SignedString(a.JWTSecret)
	if err != nil {
		log.Printf("token signing failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": signed})
}

// RequireAuth validates the bearer token and puts the advertiser id on the
// request context.
func (a *API) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))

		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return a.JWTSecret, nil
		})
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		advertiserID, _ := claims["advertiser_id"].(string)
		if advertiserID == "" {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), advertiserIDKey, advertiserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func advertiserFromContext(ctx context.Context) string {
	id, _ := ctx.Value(advertiserIDKey).(string)
	return id
}
