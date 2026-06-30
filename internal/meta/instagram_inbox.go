package meta

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Instagram Messaging webhook. Unlike WhatsApp, Instagram exposes a conversation
// + message-history API (see IGConversations / IGMessages), so we don't persist
// anything: the webhook is purely a realtime trigger. On each inbound DM Meta
// POSTs here, we drop the conversation cache and bump the hub, and the dashboard
// (connected over /api/meta/instagram/ws) refetches the live threads from Graph.

// IGWebhookVerify answers Meta's GET subscription handshake: echo hub.challenge
// when hub.verify_token matches ours. Public (Meta calls it, no JWT).
func (h *MetaHandler) IGWebhookVerify(c *gin.Context) {
	mode := c.Query("hub.mode")
	token := c.Query("hub.verify_token")
	challenge := c.Query("hub.challenge")
	if mode == "subscribe" && token != "" && token == h.igVerifyToken {
		c.String(http.StatusOK, challenge)
		return
	}
	c.String(http.StatusForbidden, "verification failed")
}

// igWebhookEnvelope mirrors the slice of the Instagram Messaging webhook payload
// we care about. Inbound DMs arrive under entry[].messaging[]; our own sends come
// back as echoes (message.is_echo) which we ignore so a reply doesn't self-trigger.
type igWebhookEnvelope struct {
	Object string `json:"object"`
	Entry  []struct {
		ID        string `json:"id"`
		Messaging []struct {
			Sender  struct {
				ID string `json:"id"`
			} `json:"sender"`
			Message struct {
				Mid    string `json:"mid"`
				Text   string `json:"text"`
				IsEcho bool   `json:"is_echo"`
			} `json:"message"`
		} `json:"messaging"`
		Changes []struct {
			Field string `json:"field"`
		} `json:"changes"`
	} `json:"entry"`
}

// IGWebhookReceive ingests inbound IG DM events. It always 200s fast (Meta retries
// on non-200 and disables a slow/erroring webhook). Authenticity is checked via
// the app-secret signature when set. No persistence — it just invalidates the IG
// cache and bumps the hub so connected dashboards refetch.
func (h *MetaHandler) IGWebhookReceive(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Status(http.StatusOK)
		return
	}
	if !h.validSignature(c.GetHeader("X-Hub-Signature-256"), body) {
		c.String(http.StatusForbidden, "bad signature")
		return
	}
	var env igWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		c.Status(http.StatusOK)
		return
	}
	anyNew := false
	for _, e := range env.Entry {
		for _, m := range e.Messaging {
			if m.Message.Mid != "" && !m.Message.IsEcho {
				anyNew = true
			}
		}
		if len(e.Changes) > 0 { // field-style delivery (e.g. "messages")
			anyNew = true
		}
	}
	if anyNew {
		h.invalidateIGCache()
		if h.igHub != nil {
			h.igHub.Bump()
		}
	}
	c.Status(http.StatusOK)
}
