package meta

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"metaapi/internal/store"

	"github.com/gin-gonic/gin"
)

// Instagram-login account management. The "Instagram API with Instagram Login"
// has no System User token — each professional account is authorised with its own
// user access token. The team pastes a long-lived token (60 days, from the App
// Dashboard "Create token" button) here; the server stores it and refreshes it in
// place before expiry so it effectively never dies.

// igAccountTokenTTL is the long-lived token lifetime we assume on connect when
// Meta doesn't tell us otherwise.
const igAccountTokenTTL = 60 * 24 * time.Hour

type igConnectRequest struct {
	AccessToken string `json:"access_token"`
}

// IGConnect validates a pasted Instagram-login token against /me, upgrades it to
// long-lived when possible, and stores (or refreshes) the account.
func (h *MetaHandler) IGConnect(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store belum aktif"})
		return
	}
	var req igConnectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.AccessToken)
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Access token wajib diisi."})
		return
	}
	// Best-effort upgrade short-lived → long-lived (needs the app secret).
	token = h.igExchangeLongLived(token)

	acc, err := h.igMe(token)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Token tidak valid / gagal membaca akun Instagram: " + err.Error()})
		return
	}
	if acc.IGUserID == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Token tidak mengembalikan ID akun Instagram."})
		return
	}
	acc.AccessToken = token
	exp := time.Now().Add(igAccountTokenTTL)
	now := time.Now()
	acc.TokenExpiresAt = &exp
	acc.RefreshedAt = &now
	if err := h.wa.UpsertIGAccount(acc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.invalidateIGCache()
	if h.igHub != nil {
		h.igHub.Bump()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "account": gin.H{"id": acc.IGUserID, "username": acc.Username}})
}

// IGAccounts lists connected accounts (tokens never serialised) for management.
func (h *MetaHandler) IGAccounts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"accounts": h.igAccounts()})
}

// IGDisconnect removes a connected account by its DB id.
func (h *MetaHandler) IGDisconnect(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store belum aktif"})
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return
	}
	if err := h.wa.DeleteIGAccount(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.invalidateIGCache()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// igMe reads the profile of the account a token authorises (graph.instagram.com).
func (h *MetaHandler) igMe(token string) (*store.IGAccount, error) {
	mc := h.igClient(token, &http.Client{Timeout: 15 * time.Second})
	r, err := mc.graph("/me", map[string]string{
		"fields": "user_id,username,name,profile_picture_url,followers_count,media_count",
	})
	if err != nil {
		return nil, err
	}
	id := gstr(r, "user_id")
	if id == "" {
		id = gstr(r, "id")
	}
	return &store.IGAccount{
		IGUserID:       id,
		Username:       gstr(r, "username"),
		Name:           gstr(r, "name"),
		ProfilePicture: gstr(r, "profile_picture_url"),
		Followers:      int(gnum(r, "followers_count")),
		MediaCount:     int(gnum(r, "media_count")),
	}, nil
}

// igExchangeLongLived swaps a short-lived token for a 60-day long-lived one. No-op
// (returns the original) when the app secret is unset or the call fails (e.g. the
// token is already long-lived, as the App Dashboard "Create token" yields).
func (h *MetaHandler) igExchangeLongLived(token string) string {
	if h.appSecret == "" {
		return token
	}
	u := "https://graph.instagram.com/access_token?grant_type=ig_exchange_token&client_secret=" +
		url.QueryEscape(h.appSecret) + "&access_token=" + url.QueryEscape(token)
	resp, err := h.http.Get(u)
	if err != nil {
		return token
	}
	defer resp.Body.Close()
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		if t := gstr(out, "access_token"); t != "" {
			return t
		}
	}
	return token
}

// StartIGTokenRefresher keeps every connected token alive forever: it runs on
// startup and every `interval`, refreshing any token within 15 days of expiry via
// the IG refresh endpoint (each refresh yields a fresh 60-day token). Spawns its
// own goroutine; no-op if the store/interval are unset.
func (h *MetaHandler) StartIGTokenRefresher(interval time.Duration) {
	if h.wa == nil || interval <= 0 {
		return
	}
	go func() {
		for {
			h.refreshIGTokens()
			time.Sleep(interval)
		}
	}()
}

// refreshIGTokens extends tokens nearing expiry. IG long-lived tokens can be
// refreshed once they're >24h old and before they expire; we refresh inside a
// 15-day safety window so a daily tick never lets one lapse.
func (h *MetaHandler) refreshIGTokens() {
	accs, err := h.wa.ListIGAccounts()
	if err != nil {
		return
	}
	for _, a := range accs {
		if a.AccessToken == "" {
			continue
		}
		if a.TokenExpiresAt != nil && time.Until(*a.TokenExpiresAt) > 15*24*time.Hour {
			continue
		}
		u := "https://graph.instagram.com/refresh_access_token?grant_type=ig_refresh_token&access_token=" +
			url.QueryEscape(a.AccessToken)
		resp, err := h.http.Get(u)
		if err != nil {
			continue
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		t := gstr(out, "access_token")
		if t == "" {
			continue
		}
		exp := time.Now().Add(igAccountTokenTTL)
		if secs := gnum(out, "expires_in"); secs > 0 {
			exp = time.Now().Add(time.Duration(secs) * time.Second)
		}
		now := time.Now()
		acc := a
		acc.AccessToken = t
		acc.TokenExpiresAt = &exp
		acc.RefreshedAt = &now
		_ = h.wa.SaveIGAccount(&acc)
	}
}
