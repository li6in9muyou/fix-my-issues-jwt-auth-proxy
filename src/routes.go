package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dgrijalva/jwt-go"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/go-playground/validator"
	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Route interface {
	setupRoutes(s *mux.Router)
}

type contextKey string

func (c contextKey) String() string {
	return "jwt-auth-proxy context key " + string(c)
}

var (
	contextKeyUserID     = contextKey("UserID")
	contextKeyAuthHeader = contextKey("AuthHeader")
)

func SendNotFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
}

func SendBadRequest(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
}

func SendUnauthorized(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnauthorized)
}

func SendAleadyExists(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
}

func SendCreated(w http.ResponseWriter, id primitive.ObjectID) {
	w.Header().Set("X-Object-ID", id.Hex())
	w.WriteHeader(http.StatusCreated)
}

func SendUpdated(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func SendInternalServerError(w http.ResponseWriter) {
	w.WriteHeader(http.StatusInternalServerError)
}

func SendJSON(w http.ResponseWriter, v interface{}) {
	json, err := json.Marshal(v)
	if err != nil {
		log.Println(err)
		SendInternalServerError(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(json)
}

func UnmarshalBody(r *http.Request, o interface{}) error {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(body, &o); err != nil {
		return err
	}
	return nil
}

func UnmarshalValidateBody(r *http.Request, o interface{}) error {
	err := UnmarshalBody(r, &o)
	if err != nil {
		return err
	}
	v := validator.New()
	err = v.Struct(o)
	if err != nil {
		return err
	}
	return nil
}

func GetUserIDFromContext(r *http.Request) string {
	userID := r.Context().Value(contextKeyUserID)
	if userID == nil {
		return ""
	}
	return userID.(string)
}

func GetAuthHeaderFromContext(r *http.Request) string {
	authHeader := r.Context().Value(contextKeyAuthHeader)
	if authHeader == nil {
		return ""
	}
	return authHeader.(string)
}

func SetCorsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", GetConfig().CorsOrigin)
	w.Header().Set("Access-Control-Allow-Headers", GetConfig().CorsHeaders)
}

func CorsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetCorsHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func ExtractClaimsFromRequest(r *http.Request) (*Claims, string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, "", errors.New("JWT header verification failed: missing auth header")
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, "", errors.New("JWT header verification failed: invalid auth header")
	}
	authHeader = strings.TrimPrefix(authHeader, "Bearer ")
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(authHeader, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(GetConfig().JwtSigningKey), nil
	})
	if err != nil {
		return nil, "", errors.New("JWT header verification failed: parsing JWT failed with: " + err.Error())
	}
	if !token.Valid {
		return nil, "", errors.New("JWT header verification failed: invalid JWT")
	}
	log.Println("Successfully verified JWT header for UserID", claims.UserID)
	return claims, authHeader, nil
}

func VerifyJwtMiddleware(next http.Handler) http.Handler {
	var isWhitelistMatch = func(url string, whitelistedURL string) bool {
		whitelistedURL = strings.TrimSpace(whitelistedURL)
		if strings.HasSuffix(whitelistedURL, "/") {
			whitelistedURL = whitelistedURL[:len(whitelistedURL)-1]
		}
		if whitelistedURL != "" && (url == whitelistedURL || strings.HasPrefix(url, whitelistedURL+"/")) {
			return true
		}
		return false
	}

	var IsWhitelisted = func(r *http.Request) bool {
		url := r.URL.EscapedPath()
		// Check for whitelisted public API paths
		for _, whitelistedURL := range unauthorizedRoutes {
			if isWhitelistMatch(url, whitelistedURL) {
				return true
			}
		}
		// All other public API paths require a valid auth token
		if strings.HasPrefix(url, GetConfig().PublicAPIPath) {
			return false
		}
		// Whitelist Mode: Check is URL is whitelisted, else assume auth token is required
		if len(GetConfig().ProxyWhitelist) > 0 {
			for _, whitelistedURL := range GetConfig().ProxyWhitelist {
				if isWhitelistMatch(url, whitelistedURL) {
					return true
				}
			}
			return false
		}
		// Blacklist Mode: Check is URL is blacklisted, else assume auth token is NOT required
		for _, blacklistedURL := range GetConfig().ProxyBlacklist {
			if isWhitelistMatch(url, blacklistedURL) {
				return false
			}
		}
		return true
	}

	var HandleWhitelistReq = func(w http.ResponseWriter, r *http.Request) {
		claims, authHeader, err := ExtractClaimsFromRequest(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), contextKeyUserID, claims.UserID)
		ctx = context.WithValue(ctx, contextKeyAuthHeader, authHeader)
		next.ServeHTTP(w, r.WithContext(ctx))
	}

	var HandleNonWhitelistReq = func(w http.ResponseWriter, r *http.Request) {
		claims, authHeader, err := ExtractClaimsFromRequest(r)
		if err != nil {
			log.Println(err)
			SendUnauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), contextKeyUserID, claims.UserID)
		ctx = context.WithValue(ctx, contextKeyAuthHeader, authHeader)
		next.ServeHTTP(w, r.WithContext(ctx))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			HandleWhitelistReq(w, r)
		} else if IsWhitelisted(r) {
			HandleWhitelistReq(w, r)
		} else {
			HandleNonWhitelistReq(w, r)
		}
	})
}

func CorsHandler(w http.ResponseWriter, r *http.Request) {
	SetCorsHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func ProxyHandler(w http.ResponseWriter, r *http.Request) {
	var getScheme = func(s string) string {
		if r.URL.Scheme == "" {
			return "http"
		}
		return r.URL.Scheme
	}

	url := r.URL.RequestURI()
	log.Println("Proxying request for", url)

	r.Header.Set("X-Forwarded-For", r.RemoteAddr)
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Proto", getScheme(r.URL.Scheme))
	r.Header.Set("Forwarded", fmt.Sprintf("for=%s;host=%s;proto=%s", r.RemoteAddr, r.Host, getScheme(r.URL.Scheme)))
	r.Header.Set("X-Auth-UserID", GetUserIDFromContext(r))
	r.Header.Del("Authorization")
	authHeader := GetAuthHeaderFromContext(r)
	if authHeader != "" {
		r.Header.Set("Authorization", "Bearer "+authHeader)
	}

	target := GetConfig().ProxyTarget
	r.URL.Host = target.Host
	r.URL.Scheme = target.Scheme
	r.Host = target.Host

	GetApp().Proxy.ServeHTTP(w, r)
}

var unauthorizedRoutes = [...]string{
	GetConfig().PublicAPIPath + "login",
	GetConfig().PublicAPIPath + "signup",
	GetConfig().PublicAPIPath + "confirm",
	GetConfig().PublicAPIPath + "initpwreset",
}
