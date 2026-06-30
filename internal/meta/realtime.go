package meta

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"metaapi/internal/auth"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// Hub pushes a "revision bump" to every connected dashboard whenever a new
// WhatsApp message arrives via the webhook, so the inbox updates instantly with
// no polling. The browser re-fetches conversations/messages on each push.
type Hub struct {
	rev   int64
	mu    sync.Mutex
	conns map[*websocket.Conn]bool
}

func NewHub() *Hub { return &Hub{conns: map[*websocket.Conn]bool{}} }

func (h *Hub) revision() int64 { return atomic.LoadInt64(&h.rev) }

// Bump increments the revision and fans it out to every connected client.
func (h *Hub) Bump() {
	rev := atomic.AddInt64(&h.rev, 1)
	msg := map[string]int64{"rev": rev}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteJSON(msg); err != nil {
			delete(h.conns, c)
			_ = c.Close()
		}
	}
}

func (h *Hub) add(c *websocket.Conn) {
	h.mu.Lock()
	h.conns[c] = true
	h.mu.Unlock()
}

func (h *Hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	_ = c.Close()
}

func (h *Hub) sendRev(c *websocket.Conn, rev int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = c.WriteJSON(map[string]int64{"rev": rev})
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // same-trust setup behind our proxy
}

// ServeWS upgrades to a WebSocket after validating the token (passed as a query
// param — browsers can't set headers on a WS handshake). Accepts the same
// tokens as the HTTP middleware (SSO EdDSA or legacy HS256).
func (h *Hub) ServeWS(secret string, sso *auth.SSOVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auth.TokenValid(secret, sso, c.Query("token")) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		h.add(conn)
		h.sendRev(conn, h.revision()) // sync immediately on connect
		go func() {
			defer h.remove(conn)
			conn.SetReadLimit(512)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}
}
