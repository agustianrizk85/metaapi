package meta

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"metaapi/internal/store"

	"github.com/gin-gonic/gin"
)

// WhatsApp Cloud API inbox. Unlike Instagram, WhatsApp offers no endpoint to
// read conversation history — inbound messages are delivered once, via webhook.
// So we capture them (WebhookReceive), persist (store), and the dashboard reads
// the thread list / history back from the DB. Replies go out through the Cloud
// API send endpoint and are persisted too, so both sides show in the inbox.

// WebhookVerify answers Meta's GET subscription handshake: echo hub.challenge
// when hub.verify_token matches ours. Public (Meta calls it, no JWT).
func (h *MetaHandler) WebhookVerify(c *gin.Context) {
	mode := c.Query("hub.mode")
	token := c.Query("hub.verify_token")
	challenge := c.Query("hub.challenge")
	if mode == "subscribe" && token != "" && token == h.waVerifyToken {
		c.String(http.StatusOK, challenge)
		return
	}
	c.String(http.StatusForbidden, "verification failed")
}

// webhookEnvelope mirrors the slice of the WhatsApp webhook payload we use.
type webhookEnvelope struct {
	Entry []struct {
		ID      string `json:"id"` // WABA id
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Button struct {
						Text string `json:"text"`
					} `json:"button"`
				} `json:"messages"`
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// WebhookReceive ingests inbound messages + delivery statuses. It always 200s
// fast (Meta retries on non-200, and a slow/erroring webhook gets disabled).
// Public route; authenticity is checked via the app-secret signature when set.
func (h *MetaHandler) WebhookReceive(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Status(http.StatusOK)
		return
	}
	if !h.validSignature(c.GetHeader("X-Hub-Signature-256"), body) {
		c.String(http.StatusForbidden, "bad signature")
		return
	}
	if h.wa == nil {
		c.Status(http.StatusOK)
		return
	}
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		c.Status(http.StatusOK)
		return
	}
	for _, e := range env.Entry {
		for _, ch := range e.Changes {
			v := ch.Value
			// Customer display names keyed by wa_id.
			names := map[string]string{}
			for _, ct := range v.Contacts {
				if ct.WaID != "" {
					names[ct.WaID] = ct.Profile.Name
				}
			}
			for _, m := range v.Messages {
				_, _ = h.wa.SaveIncoming(&store.WAMessage{
					WamID:         m.ID,
					PhoneNumberID: v.Metadata.PhoneNumberID,
					WabaID:        e.ID,
					ContactWaID:   m.From,
					ContactName:   names[m.From],
					Type:          m.Type,
					Text:          messageText(m.Type, m.Text.Body, m.Button.Text),
					Timestamp:     unixToTime(m.Timestamp),
				})
			}
			for _, st := range v.Statuses {
				_ = h.wa.UpdateStatus(st.ID, st.Status)
			}
		}
	}
	c.Status(http.StatusOK)
}

// validSignature verifies Meta's X-Hub-Signature-256 (sha256=<hmac>) over the
// raw body using the app secret. When no app secret is configured we accept
// (dev / not-yet-set) — set META_APP_SECRET in production to enforce it.
func (h *MetaHandler) validSignature(header string, body []byte) bool {
	if h.appSecret == "" {
		return true
	}
	want := strings.TrimPrefix(header, "sha256=")
	mac := hmac.New(sha256.New, []byte(h.appSecret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(got))
}

// WAConversations lists inbox threads (newest first), optionally scoped to one
// of our phone numbers via ?phone_number_id=.
func (h *MetaHandler) WAConversations(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"configured": false, "conversations": []any{}})
		return
	}
	convs, err := h.wa.Conversations(c.Query("phone_number_id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "conversations": convs})
}

// WAMessages returns one thread's history and marks it read.
func (h *MetaHandler) WAMessages(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"configured": false, "messages": []any{}})
		return
	}
	pnid := c.Query("phone_number_id")
	contact := c.Query("contact")
	if pnid == "" || contact == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone_number_id & contact wajib"})
		return
	}
	msgs, err := h.wa.Messages(pnid, contact, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.wa.MarkRead(pnid, contact)
	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}

// WASend sends a free-form text reply via the Cloud API and stores it. Meta
// only allows free-form text inside the 24h customer-service window; outside it
// the send fails and the error (which mentions a template is required) is
// surfaced verbatim.
func (h *MetaHandler) WASend(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WhatsApp inbox belum aktif"})
		return
	}
	var req struct {
		PhoneNumberID string `json:"phone_number_id"`
		To            string `json:"to"`
		Text          string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.To = strings.TrimSpace(req.To)
	req.Text = strings.TrimSpace(req.Text)
	if req.PhoneNumberID == "" || req.To == "" || req.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone_number_id, to, dan text wajib"})
		return
	}
	mc, ok := h.clientForPhone(req.PhoneNumberID)
	if !ok || !mc.configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "token Meta belum diset / tidak ada akun yang mengakses nomor ini"})
		return
	}
	res, err := mc.graphPost("/"+req.PhoneNumberID+"/messages", map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                req.To,
		"type":              "text",
		"text":              map[string]any{"preview_url": false, "body": req.Text},
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Echo the sent message into the thread so the UI shows it immediately.
	wamID := ""
	if arr, ok := res["messages"].([]any); ok && len(arr) > 0 {
		if m0, ok := arr[0].(map[string]any); ok {
			wamID = gstr(m0, "id")
		}
	}
	_ = h.wa.SaveOutgoing(&store.WAMessage{
		WamID:         wamID,
		PhoneNumberID: req.PhoneNumberID,
		ContactWaID:   req.To,
		Type:          "text",
		Text:          req.Text,
		Status:        "sent",
		Timestamp:     time.Now(),
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "wamId": wamID})
}

// messageText extracts a human-readable body from a webhook message, falling
// back to a type placeholder for non-text messages (image/audio/…).
func messageText(typ, textBody, buttonText string) string {
	if textBody != "" {
		return textBody
	}
	if buttonText != "" {
		return buttonText
	}
	if typ != "" && typ != "text" {
		return "[" + typ + "]"
	}
	return ""
}

// unixToTime parses Meta's string unix-seconds timestamp; falls back to now.
func unixToTime(s string) time.Time {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Now()
}
