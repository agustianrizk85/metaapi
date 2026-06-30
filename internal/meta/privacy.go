package meta

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// Meta App Review requires a Deauthorize callback and a Data Deletion Request
// callback (under "Business login settings"). Meta POSTs a signed_request to each
// when a user removes the app or asks for their data to be deleted. We have no
// end-user data (the app uses our own business tokens), so these handlers verify
// the request, best-effort drop any matching stored account, and answer in Meta's
// required format. All public (Meta calls them, no JWT).

// parseSignedRequest decodes and (when an app secret is set) verifies Meta's
// signed_request "sig.payload" form, returning the JSON payload.
func parseSignedRequest(signed, secret string) (map[string]any, error) {
	parts := strings.SplitN(signed, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("bad signed_request")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	if secret != "" {
		sig, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(parts[1]))
		if !hmac.Equal(sig, mac.Sum(nil)) {
			return nil, fmt.Errorf("bad signature")
		}
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// dropAccountByUserID removes a stored IG account whose id matches the
// signed_request user (best effort — we may not store that user at all).
func (h *MetaHandler) dropAccountByUserID(uid string) {
	if uid == "" || h.wa == nil {
		return
	}
	if a := h.findIGAccount(uid); a != nil {
		_ = h.wa.DeleteIGAccount(a.ID)
		h.invalidateIGCache()
	}
}

// Deauthorize handles Meta's app-removal callback: drop any matching account, 200.
func (h *MetaHandler) Deauthorize(c *gin.Context) {
	if data, err := parseSignedRequest(c.PostForm("signed_request"), h.appSecret); err == nil {
		h.dropAccountByUserID(gstr(data, "user_id"))
	}
	c.Status(http.StatusOK)
}

// DataDeletion handles Meta's data-deletion request: drop any matching account
// and return the status URL + confirmation code Meta requires.
func (h *MetaHandler) DataDeletion(c *gin.Context) {
	code := "gp-none"
	if data, err := parseSignedRequest(c.PostForm("signed_request"), h.appSecret); err == nil {
		if uid := gstr(data, "user_id"); uid != "" {
			code = "gp-" + uid
			h.dropAccountByUserID(uid)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"url":               "https://meta.greenparkgroup.cloud/api/meta/data-deletion-status?code=" + url.QueryEscape(code),
		"confirmation_code": code,
	})
}

// DataDeletionStatus is the human/Meta-checkable status page for a deletion code.
func (h *MetaHandler) DataDeletionStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":            "completed",
		"confirmation_code": c.Query("code"),
		"message":           "Permintaan penghapusan data telah diproses. Aplikasi ini tidak menyimpan data pesan pengguna.",
	})
}
