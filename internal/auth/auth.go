// Package auth provides JWT validation only. There is no user store or login
// here: metaapi trusts tokens issued by the marketing backend
// (greenparkmarketingbee) by validating them against the SAME shared JWT secret.
// One unified login — the frontend authenticates against the marketing backend
// and reuses that token for these Meta endpoints.
package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Middleware validates the Bearer token. It accepts EITHER:
//   - an Ed25519 (EdDSA) SSO token from the master auth service (verified via its
//     JWKS public key) — this is what the unified dashboard login issues; or
//   - a legacy HS256 token signed with the shared secret (marketing backend).
// Whichever validates first wins, so both the dashboard SSO login and the older
// per-backend tokens keep working. sso may be nil (HS256 only).
func Middleware(secret string, sso *SSOVerifier) gin.HandlerFunc {
	key := []byte(secret)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		raw := strings.TrimPrefix(header, "Bearer ")

		// EdDSA SSO token (dashboard login) — try first when configured.
		if sso != nil {
			if _, err := sso.Verify(raw); err == nil {
				c.Next()
				return
			}
		}
		// Legacy HS256 token (shared secret).
		tok, err := jwt.Parse(raw, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return key, nil
		})
		if err == nil && tok.Valid {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
	}
}
