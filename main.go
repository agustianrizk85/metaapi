// metaapi — a small, self-contained Meta (Facebook/Instagram) Graph proxy.
//
// It exposes the Ads / WhatsApp / Instagram (incl. DM inbox) endpoints used by
// the Greenpark marketing dashboard, authenticated with JWT, and can optionally
// serve the built SPA so a single binary powers meta.greenparkgroup.cloud.
//
// No database: the Meta credential is one System User token from the env.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"metaapi/internal/auth"
	"metaapi/internal/config"
	"metaapi/internal/meta"
	"metaapi/internal/store"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.Load()
	if cfg.MetaToken == "" {
		log.Println("WARNING: META_ACCESS_TOKEN is empty — Meta endpoints will report configured:false")
	}

	metaH := meta.NewMetaHandler(cfg.MetaToken, cfg.MetaAPIVersion, cfg.MetaBusinessID, cfg.MetaAdAccount)

	// WhatsApp inbox storage — incoming messages arrive only via webhook, so we
	// persist them. If the DB can't open we log and run without the inbox rather
	// than crash the Graph proxy.
	if st, err := store.Open(cfg.DBPath); err != nil {
		log.Printf("WARNING: WhatsApp inbox disabled — DB open failed: %v", err)
	} else {
		metaH.EnableWhatsAppInbox(st, cfg.WAWebhookVerifyToken, cfg.MetaAppSecret)
		log.Printf("WhatsApp inbox enabled (db=%s)", cfg.DBPath)
	}

	r := gin.Default()
	r.Use(corsMiddleware(cfg.CORSOrigins))

	api := r.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

		// WhatsApp Cloud API webhook — PUBLIC (Meta calls it, no JWT). GET is the
		// subscription handshake; POST delivers inbound messages + statuses
		// (authenticity checked via the app-secret signature).
		api.GET("/meta/whatsapp/webhook", metaH.WebhookVerify)
		api.POST("/meta/whatsapp/webhook", metaH.WebhookReceive)

		// Auth is unified with the marketing backend: tokens it issues are
		// validated here against the shared JWT secret. metaapi has no login.
		authed := api.Group("")
		authed.Use(auth.Middleware(cfg.JWTSecret))
		{
			authed.GET("/meta/ads", metaH.Ads)
			authed.GET("/meta/ads/detail", metaH.AdsDetail)
			authed.GET("/meta/ads/campaign", metaH.AdsCampaign)
			authed.GET("/meta/whatsapp", metaH.WhatsApp)
			authed.GET("/meta/whatsapp/conversations", metaH.WAConversations)
			authed.GET("/meta/whatsapp/messages", metaH.WAMessages)
			authed.POST("/meta/whatsapp/send", metaH.WASend)
			authed.GET("/meta/instagram", metaH.Instagram)
			authed.GET("/meta/instagram/conversations", metaH.IGConversations)
			authed.GET("/meta/instagram/messages", metaH.IGMessages)
			authed.POST("/meta/instagram/send", metaH.IGSend)
		}
	}

	// Optionally serve the built SPA so one binary serves the whole site.
	if cfg.FrontendDir != "" {
		serveSPA(r, cfg.FrontendDir)
	}

	addr := ":" + cfg.AppPort
	log.Printf("metaapi listening on %s (frontend=%q)", addr, cfg.FrontendDir)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// corsMiddleware allows all origins when CORS_ORIGINS is empty, else the listed
// comma-separated origins.
func corsMiddleware(origins string) gin.HandlerFunc {
	c := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
	}
	if strings.TrimSpace(origins) == "" {
		c.AllowAllOrigins = true
	} else {
		for _, o := range strings.Split(origins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				c.AllowOrigins = append(c.AllowOrigins, o)
			}
		}
	}
	return cors.New(c)
}

// serveSPA serves static files from dir, falling back to index.html for client
// routes. /api/* paths are never intercepted (they 404 as JSON if unmatched).
func serveSPA(r *gin.Engine, dir string) {
	index := filepath.Join(dir, "index.html")
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// Serve the requested static asset when it exists, else the SPA shell.
		clean := filepath.Join(dir, filepath.Clean("/"+p))
		if !strings.HasPrefix(clean, filepath.Clean(dir)) { // path traversal guard
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad path"})
			return
		}
		if info, err := os.Stat(clean); err == nil && !info.IsDir() {
			c.File(clean)
			return
		}
		c.File(index)
	})
}
