package auth

// SSO verifier — validates Ed25519 (EdDSA) access tokens issued by the Greenpark
// master auth service, using its public keys published at the JWKS endpoint. This
// is the same scheme as the shared `authmw` integration package, trimmed to just
// token verification (metaapi gates by signature/expiry, not by department). It
// lets the unified dashboard's own login token drive the Meta endpoints — no
// per-backend token bridge needed.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// SSOClaims mirrors the auth service's access-token payload (only what we use).
type SSOClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Username  string `json:"username"`
	ExpiresAt int64  `json:"exp"`
}

// SSOVerifier verifies EdDSA tokens against the auth service's JWKS, caching keys.
type SSOVerifier struct {
	jwksURL string
	issuer  string
	client  *http.Client

	mu   sync.RWMutex
	keys map[string]ed25519.PublicKey // kid -> key
}

// NewSSOVerifier builds a verifier; keys are fetched lazily on first use. Returns
// nil when no JWKS URL is configured (SSO acceptance simply stays off).
func NewSSOVerifier(jwksURL, issuer string) *SSOVerifier {
	if strings.TrimSpace(jwksURL) == "" {
		return nil
	}
	return &SSOVerifier{
		jwksURL: jwksURL,
		issuer:  issuer,
		client:  &http.Client{Timeout: 5 * time.Second},
		keys:    map[string]ed25519.PublicKey{},
	}
}

// Verify validates a compact EdDSA JWT and returns its claims.
func (v *SSOVerifier) Verify(tok string) (SSOClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return SSOClaims{}, errors.New("token tidak valid")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil {
		return SSOClaims{}, errors.New("token tidak valid")
	}
	if h.Alg != "EdDSA" {
		return SSOClaims{}, fmt.Errorf("alg %q tidak didukung", h.Alg)
	}
	key, err := v.keyFor(h.Kid)
	if err != nil {
		return SSOClaims{}, err
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return SSOClaims{}, errors.New("token tidak valid")
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return SSOClaims{}, errors.New("tanda tangan token tidak cocok")
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return SSOClaims{}, errors.New("token tidak valid")
	}
	var c SSOClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return SSOClaims{}, errors.New("token tidak valid")
	}
	if c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt {
		return SSOClaims{}, errors.New("token kedaluwarsa")
	}
	if v.issuer != "" && c.Issuer != v.issuer {
		return SSOClaims{}, errors.New("issuer token tidak dikenal")
	}
	return c, nil
}

func (v *SSOVerifier) keyFor(kid string) (ed25519.PublicKey, error) {
	v.mu.RLock()
	if k, ok := v.keys[kid]; ok {
		v.mu.RUnlock()
		return k, nil
	}
	v.mu.RUnlock()
	if err := v.fetchJWKS(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("kunci verifikasi (kid) tidak dikenal")
}

func (v *SSOVerifier) fetchJWKS() error {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return fmt.Errorf("ambil JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ambil JWKS: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}
	fresh := make(map[string]ed25519.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		raw, err := b64.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		fresh[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(fresh) == 0 {
		return errors.New("JWKS tidak berisi kunci Ed25519")
	}
	v.mu.Lock()
	v.keys = fresh
	v.mu.Unlock()
	return nil
}
