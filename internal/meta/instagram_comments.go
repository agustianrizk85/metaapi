package meta

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// IGMedia lists an IG account's recent posts so the app can pick which post's
// comments to moderate. Query: ?account=<ig_user_id>.
func (h *MetaHandler) IGMedia(c *gin.Context) {
	acc := h.findIGAccount(strings.TrimSpace(c.Query("account")))
	if acc == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "akun IG tidak ditemukan / token belum ditambah"})
		return
	}
	mc := h.igClient(acc.AccessToken, nil)
	res, err := mc.graph("/me/media", map[string]string{
		"fields": "id,caption,media_type,media_url,thumbnail_url,permalink,timestamp,comments_count,like_count",
		"limit":  "25",
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"media": res["data"]})
}

// IGComments lists the comments on one media (with nested replies).
// Query: ?account=<ig_user_id>&media_id=<id>.
func (h *MetaHandler) IGComments(c *gin.Context) {
	acc := h.findIGAccount(strings.TrimSpace(c.Query("account")))
	mediaID := strings.TrimSpace(c.Query("media_id"))
	if acc == nil || mediaID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account & media_id wajib"})
		return
	}
	mc := h.igClient(acc.AccessToken, nil)
	res, err := mc.graph("/"+mediaID+"/comments", map[string]string{
		"fields": "id,text,username,timestamp,like_count,replies{id,text,username,timestamp}",
		"limit":  "50",
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"comments": res["data"]})
}

// IGCommentReply posts a public reply to a comment. Body: {account, comment_id, message}.
func (h *MetaHandler) IGCommentReply(c *gin.Context) {
	var req struct {
		Account   string `json:"account"`
		CommentID string `json:"comment_id"`
		Message   string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	acc := h.findIGAccount(strings.TrimSpace(req.Account))
	if acc == nil || strings.TrimSpace(req.CommentID) == "" || strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account, comment_id, message wajib"})
		return
	}
	mc := h.igClient(acc.AccessToken, nil)
	res, err := mc.graphPost("/"+req.CommentID+"/replies", map[string]any{"message": req.Message})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "result": res})
}
