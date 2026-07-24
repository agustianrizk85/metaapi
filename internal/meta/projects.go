package meta

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"metaapi/internal/auth"
	"metaapi/internal/store"
)

// MyProjects returns only the projects the CALLER is assigned to as sales
// (matched by the SSO login e-mail). For the field-sales Android app so a sales
// person sees just their own projects + attributed chats. Empty e-mail (legacy
// token) ⇒ empty list (use /meta/projects for the full list).
func (h *MetaHandler) MyProjects(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"projects": []any{}, "email": ""})
		return
	}
	email := auth.CurrentEmail(c)
	all, err := h.wa.ListProjects()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	mine := make([]store.Project, 0, len(all))
	if email != "" {
		for _, p := range all {
			for _, s := range p.Sales {
				if strings.EqualFold(strings.TrimSpace(s.Email), email) {
					mine = append(mine, p)
					break
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"projects": mine, "email": email})
}

// Projects lists every project with its linked WA/IG accounts + sales team.
func (h *MetaHandler) Projects(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"projects": []any{}})
		return
	}
	ps, err := h.wa.ListProjects()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": ps})
}

// SaveProject creates or updates a project (by id) and replaces its account +
// sales links with the supplied full set.
func (h *MetaHandler) SaveProject(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "penyimpanan belum aktif"})
		return
	}
	var p store.Project
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nama proyek wajib diisi"})
		return
	}
	// Normalise links (drop blanks, default kind).
	accounts := p.Accounts[:0]
	for _, a := range p.Accounts {
		a.Ref = strings.TrimSpace(a.Ref)
		if a.Ref == "" {
			continue
		}
		if a.Kind != "ig" {
			a.Kind = "wa"
		}
		accounts = append(accounts, a)
	}
	p.Accounts = accounts
	sales := p.Sales[:0]
	for _, sp := range p.Sales {
		sp.Email = strings.TrimSpace(strings.ToLower(sp.Email))
		if sp.Email == "" {
			continue
		}
		sales = append(sales, sp)
	}
	p.Sales = sales
	masterNames := p.MasterNames[:0]
	seenMN := map[string]bool{}
	for _, mn := range p.MasterNames {
		mn.Name = strings.TrimSpace(mn.Name)
		if mn.Name == "" || seenMN[mn.Name] {
			continue
		}
		seenMN[mn.Name] = true
		masterNames = append(masterNames, mn)
	}
	p.MasterNames = masterNames

	if err := h.wa.SaveProject(&p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, p)
}

// DeleteProject removes a project and its links.
func (h *MetaHandler) DeleteProject(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "penyimpanan belum aktif"})
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return
	}
	if err := h.wa.DeleteProject(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
