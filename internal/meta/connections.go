package meta

import (
	"net/http"
	"strconv"
	"strings"

	"metaapi/internal/store"

	"github.com/gin-gonic/gin"
)

// Multi-account connection management. metaapi aggregates DATA across every
// stored connection (see clients()), so the team can paste several System User
// tokens here and Ads / WhatsApp / Instagram show the combined portfolio. This
// is the dynamic replacement for the single env META_ACCESS_TOKEN.

// connConfigured reports whether OAuth app credentials are set (paste-token flow
// doesn't need them).
func (h *MetaHandler) appConfigResponse(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store belum aktif"})
		return
	}
	cfg, err := h.wa.AppConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	apiVer := cfg.APIVersion
	if apiVer == "" {
		apiVer = h.ver
	}
	count, _ := h.wa.CountConnections()
	c.JSON(http.StatusOK, gin.H{
		"app_id":       cfg.AppID,
		"redirect_uri": cfg.RedirectURI,
		"api_version":  apiVer,
		"scopes":       cfg.Scopes,
		"has_secret":   cfg.AppSecret != "",
		"configured":   cfg.AppID != "" && cfg.AppSecret != "",
		"connections":  count,
	})
}

// Config returns the OAuth app config + connection count (for UI readiness).
func (h *MetaHandler) Config(c *gin.Context) { h.appConfigResponse(c) }

type saveConfigRequest struct {
	AppID       *string `json:"app_id"`
	AppSecret   *string `json:"app_secret"`
	RedirectURI *string `json:"redirect_uri"`
	APIVersion  *string `json:"api_version"`
	Scopes      *string `json:"scopes"`
}

// SaveConfig upserts OAuth app credentials (blank secret keeps the stored one).
func (h *MetaHandler) SaveConfig(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store belum aktif"})
		return
	}
	var req saveConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg, err := h.wa.AppConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if req.AppID != nil {
		cfg.AppID = strings.TrimSpace(*req.AppID)
	}
	if req.AppSecret != nil && strings.TrimSpace(*req.AppSecret) != "" {
		cfg.AppSecret = strings.TrimSpace(*req.AppSecret)
	}
	if req.RedirectURI != nil {
		cfg.RedirectURI = strings.TrimSpace(*req.RedirectURI)
	}
	if req.APIVersion != nil {
		cfg.APIVersion = strings.TrimSpace(*req.APIVersion)
	}
	if req.Scopes != nil {
		cfg.Scopes = strings.TrimSpace(*req.Scopes)
	}
	if err := h.wa.SaveAppConfig(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.appConfigResponse(c)
}

// ListConnections returns all connected accounts (tokens never serialised).
func (h *MetaHandler) ListConnections(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"connections": []any{}, "count": 0})
		return
	}
	conns, err := h.wa.ListConnections()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"connections": conns, "count": len(conns)})
}

type connectManualRequest struct {
	AccessToken string `json:"access_token"`
	Label       string `json:"label"`
}

// ConnectManual stores a pasted access token (e.g. a System User token) as a
// connected account — validated against /me, upserted by Meta user id, and made
// active. Long-lived tokens carry no expiry, so token_expires_at stays null.
func (h *MetaHandler) ConnectManual(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "store belum aktif"})
		return
	}
	var req connectManualRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.AccessToken)
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Access token wajib diisi."})
		return
	}
	me, err := h.graphMe(token)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Token tidak valid / gagal membaca akun Meta: " + err.Error()})
		return
	}
	label := strings.TrimSpace(req.Label)

	conn, ferr := h.wa.FindConnectionByMetaUserID(me.id)
	if ferr == nil && conn != nil {
		conn.AccessToken = token
		conn.TokenExpiresAt = nil
		conn.MetaUserName = me.name
		if label != "" {
			conn.Label = label
		} else if conn.Label == "" {
			conn.Label = me.name
		}
		if err := h.wa.SaveConnection(conn); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		if label == "" {
			label = me.name
		}
		conn = &store.MetaConnection{
			Label:        label,
			MetaUserID:   me.id,
			MetaUserName: me.name,
			AccessToken:  token,
		}
		if err := h.wa.CreateConnection(conn); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	_ = h.wa.SetActive(conn.ID)
	h.ListConnections(c)
}

// Activate switches which connection is used for deep single-account breakdowns.
func (h *MetaHandler) Activate(c *gin.Context) {
	id, ok := parseConnID(c)
	if !ok {
		return
	}
	if err := h.wa.SetActive(id); err != nil {
		status := http.StatusInternalServerError
		if err == store.ErrNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	h.ListConnections(c)
}

type updateConnectionRequest struct {
	Label       *string `json:"label"`
	AdAccountID *string `json:"ad_account_id"`
	BusinessID  *string `json:"business_id"`
}

// UpdateConnection edits per-account label / pinned ad account / business id.
func (h *MetaHandler) UpdateConnection(c *gin.Context) {
	id, ok := parseConnID(c)
	if !ok {
		return
	}
	var req updateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, err := h.wa.FindConnection(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	if req.Label != nil {
		conn.Label = strings.TrimSpace(*req.Label)
	}
	if req.AdAccountID != nil {
		conn.AdAccountID = strings.TrimPrefix(strings.TrimSpace(*req.AdAccountID), "act_")
	}
	if req.BusinessID != nil {
		conn.BusinessID = strings.TrimSpace(*req.BusinessID)
	}
	if err := h.wa.SaveConnection(conn); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.ListConnections(c)
}

// Disconnect removes a connected account.
func (h *MetaHandler) Disconnect(c *gin.Context) {
	id, ok := parseConnID(c)
	if !ok {
		return
	}
	if err := h.wa.DeleteConnection(id); err != nil {
		status := http.StatusInternalServerError
		if err == store.ErrNotFound {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	h.ListConnections(c)
}

func parseConnID(c *gin.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return 0, false
	}
	return uint(n), true
}

type metaIdentity struct{ id, name string }

// graphMe validates a token by reading the account it belongs to.
func (h *MetaHandler) graphMe(token string) (*metaIdentity, error) {
	mc := metaClient{token: token, ver: h.ver, http: h.http}
	res, err := mc.graph("/me", map[string]string{"fields": "id,name"})
	if err != nil {
		return nil, err
	}
	return &metaIdentity{id: gstr(res, "id"), name: gstr(res, "name")}, nil
}
