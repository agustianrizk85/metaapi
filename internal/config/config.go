package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds the runtime settings, all sourced from the environment (.env in
// dev). A single Meta System User token powers the Graph proxy; a small SQLite
// DB persists WhatsApp conversations (incoming messages arrive only via webhook).
type Config struct {
	AppPort string

	// Meta Graph credentials.
	MetaToken      string // META_ACCESS_TOKEN — System User token (long-lived)
	MetaAPIVersion string
	MetaBusinessID string
	MetaAdAccount  string // optional pinned ad account id (without act_)
	MetaAppSecret  string // META_APP_SECRET — verifies webhook X-Hub-Signature-256 (optional)

	// WhatsApp Cloud API webhook. Incoming WA messages arrive only via webhook
	// (the Graph API has no conversation-history endpoint for WhatsApp), so we
	// persist them. The verify token must match what's entered in the Meta App
	// webhook config.
	WAWebhookVerifyToken string // WA_WEBHOOK_VERIFY_TOKEN

	// Storage. metaapi gained a small DB to persist WhatsApp conversations so the
	// dashboard (and a future Android client) can read message history.
	DBPath string // WA_DB_PATH — SQLite file

	// Auth. Accepts two token types:
	//   - JWTSecret: legacy HS256 tokens (shared with the marketing backend).
	//   - AuthJWKSURL/AuthIssuer: Ed25519 SSO tokens from the master auth service
	//     (the unified dashboard login). When AuthJWKSURL is set, metaapi verifies
	//     the dashboard's own login token via auth's public keys — no token bridge.
	JWTSecret   string
	AuthJWKSURL string
	AuthIssuer  string

	// Serving.
	FrontendDir string // path to built SPA (dist) to serve; empty = API only
	CORSOrigins string // comma-separated allowed origins; empty = allow all
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Load reads .env (if present) and the environment.
func Load() *Config {
	_ = godotenv.Load()
	return &Config{
		AppPort: getEnv("APP_PORT", "8086"),

		MetaToken:      getEnv("META_ACCESS_TOKEN", ""),
		MetaAPIVersion: getEnv("META_API_VERSION", "v21.0"),
		MetaBusinessID: getEnv("META_BUSINESS_ID", ""),
		MetaAdAccount:  getEnv("META_AD_ACCOUNT_ID", ""),
		MetaAppSecret:  getEnv("META_APP_SECRET", ""),

		WAWebhookVerifyToken: getEnv("WA_WEBHOOK_VERIFY_TOKEN", "greenpark-wa-webhook"),
		DBPath:               getEnv("WA_DB_PATH", "./metaapi.db"),

		JWTSecret:   getEnv("JWT_SECRET", "dev-secret"),
		AuthJWKSURL: getEnv("AUTH_JWKS_URL", "http://127.0.0.1:8090/.well-known/jwks.json"),
		AuthIssuer:  getEnv("AUTH_ISSUER", ""), // empty = skip issuer check (sig+exp still verified)

		FrontendDir: getEnv("FRONTEND_DIR", ""),
		CORSOrigins: getEnv("CORS_ORIGINS", ""),
	}
}
