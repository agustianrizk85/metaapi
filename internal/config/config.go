package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds the runtime settings, all sourced from the environment (.env in
// dev). This service is intentionally dependency-light: no database, a single
// Meta System User token, and an optional static frontend directory.
type Config struct {
	AppPort string

	// Meta Graph credentials.
	MetaToken      string // META_ACCESS_TOKEN — System User token (long-lived)
	MetaAPIVersion string
	MetaBusinessID string
	MetaAdAccount  string // optional pinned ad account id (without act_)

	// Auth. Must match the marketing backend's JWT_SECRET so its tokens validate
	// here (unified login — metaapi issues no tokens of its own).
	JWTSecret string

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

		JWTSecret: getEnv("JWT_SECRET", "dev-secret"),

		FrontendDir: getEnv("FRONTEND_DIR", ""),
		CORSOrigins: getEnv("CORS_ORIGINS", ""),
	}
}
