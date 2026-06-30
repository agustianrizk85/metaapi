package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"metaapi/internal/store"

	"github.com/gin-gonic/gin"
)

// MetaHandler proxies the Meta (Facebook) Graph API server-side so the access
// token stays out of the browser and CORS is avoided. It powers the Ads /
// WhatsApp / Instagram (incl. DM inbox) endpoints.
//
// This standalone build uses a single System User token from the environment
// (META_ACCESS_TOKEN) — no database, no OAuth flow — so it is trivial to deploy.
type MetaHandler struct {
	envToken      string
	ver           string
	envBusinessID string
	envAdAccount  string // pinned ad account id (without act_), optional
	http          *http.Client

	// WhatsApp inbox: incoming messages arrive only via the Cloud API webhook,
	// so they're persisted here and read back for the inbox. Nil until enabled.
	wa                 *store.Store
	waVerifyToken      string // webhook verify token (must match Meta App config)
	appSecret          string // verifies webhook X-Hub-Signature-256 (optional)
	hub                *Hub   // realtime push to dashboards on new inbound (nil = off)

	// Instagram realtime: unlike WhatsApp, IG has a conversation-history API, so we
	// don't persist messages — the webhook only signals "something changed" and the
	// dashboard refetches threads from Graph. nil hub = realtime off.
	igHub         *Hub
	igVerifyToken string // IG webhook verify token (must match Meta App config)

	// Short-lived response cache: the Ads/Detail pulls fan out to dozens of
	// Graph calls (20s+). A small TTL keeps repeated loads instant.
	cmu   sync.Mutex
	cache map[string]cachedResp
}

// EnableWhatsAppInbox wires the persistence + webhook secrets for the WhatsApp
// inbox. Called once at startup after the DB is opened.
func (h *MetaHandler) EnableWhatsAppInbox(st *store.Store, verifyToken, appSecret string) {
	h.wa = st
	h.waVerifyToken = verifyToken
	h.appSecret = appSecret
}

// SetHub wires the realtime hub so the webhook can push live updates.
func (h *MetaHandler) SetHub(hub *Hub) { h.hub = hub }

// EnableInstagramRealtime wires the Instagram realtime hub + webhook verify token.
// Called once at startup; the IG webhook bumps this hub on each inbound DM.
func (h *MetaHandler) EnableInstagramRealtime(hub *Hub, verifyToken string) {
	h.igHub = hub
	h.igVerifyToken = verifyToken
}

// invalidateIGCache drops the cached IG conversation list so the next read after
// a webhook bump fetches fresh data from Graph instead of serving the 60s cache.
func (h *MetaHandler) invalidateIGCache() {
	h.cmu.Lock()
	for k := range h.cache {
		if strings.HasPrefix(k, "ig:conversations:") {
			delete(h.cache, k)
		}
	}
	h.cmu.Unlock()
}

type cachedResp struct {
	at   time.Time
	body gin.H
}

func NewMetaHandler(token, ver, businessID, adAccount string) *MetaHandler {
	if ver == "" {
		ver = "v21.0"
	}
	return &MetaHandler{envToken: token, ver: ver, envBusinessID: businessID, envAdAccount: adAccount, http: &http.Client{Timeout: 25 * time.Second}, cache: map[string]cachedResp{}}
}

// getCache returns a cached body if present and younger than ttl.
func (h *MetaHandler) getCache(key string, ttl time.Duration) (gin.H, bool) {
	h.cmu.Lock()
	defer h.cmu.Unlock()
	if e, ok := h.cache[key]; ok && time.Since(e.at) < ttl {
		return e.body, true
	}
	return nil, false
}

// setCache stores a successful response body.
func (h *MetaHandler) setCache(key string, body gin.H) {
	h.cmu.Lock()
	h.cache[key] = cachedResp{at: time.Now(), body: body}
	h.cmu.Unlock()
}

// metaClient is a request-scoped Graph client bound to the resolved credentials.
type metaClient struct {
	token      string
	ver        string
	businessID string
	adAccount  string
	label      string // connection label (Gp1/Gp2/…) — used as ad-account display name
	host       string // API host; empty = graph.facebook.com. graph.instagram.com for IG-login.
	http       *http.Client
}

// baseURL builds the versioned endpoint URL, defaulting to the Facebook Graph
// host. IG-login clients set host to graph.instagram.com.
func (mc metaClient) baseURL(path string) string {
	host := mc.host
	if host == "" {
		host = "https://graph.facebook.com"
	}
	return host + "/" + mc.ver + path
}

func (mc metaClient) configured() bool { return mc.token != "" }

// rangePreset maps the UI `range` query param to a Meta Graph `date_preset`.
// Defaults to the rolling 30-day window. "max" = maximum (lifetime, up to the
// API's ~37-month limit) so the dashboard can show campaigns across all years.
func rangePreset(c *gin.Context) string {
	switch c.Query("range") {
	case "today":
		return "today"
	case "7d":
		return "last_7d"
	case "90d":
		return "last_90d"
	case "this_year":
		return "this_year"
	case "last_year":
		return "last_year"
	case "max":
		return "maximum"
	default:
		return "last_30d"
	}
}

// clients returns ONE Graph client per connected account so every data endpoint
// aggregates across all accounts (total spend, all campaigns, all WABAs/IG). The
// accounts come ONLY from the DB-backed connections pasted in the dashboard — the
// env token is not used as a source (paste a System User token to populate this).
// Per-connection businessID overrides the env default when set.
func (h *MetaHandler) clients() []metaClient {
	var out []metaClient
	if h.wa != nil {
		if conns, err := h.wa.ListConnections(); err == nil {
			for _, cn := range conns {
				if cn.AccessToken == "" {
					continue
				}
				businessID := h.envBusinessID
				if cn.BusinessID != "" {
					businessID = cn.BusinessID
				}
				label := cn.Label
				if label == "" {
					label = cn.MetaUserName
				}
				out = append(out, metaClient{token: cn.AccessToken, ver: h.ver, businessID: businessID, adAccount: cn.AdAccountID, label: label, http: h.http})
			}
		}
	}
	return out
}

// connSig is a cache-key fragment that changes when the connection set or env
// token changes, so connect/disconnect/edit busts the short-lived cache.
func (h *MetaHandler) connSig() string {
	if h.wa == nil {
		return "env"
	}
	conns, err := h.wa.ListConnections()
	if err != nil || len(conns) == 0 {
		return "env"
	}
	parts := make([]string, 0, len(conns))
	for _, cn := range conns {
		parts = append(parts, fmt.Sprintf("%d@%d", cn.ID, cn.UpdatedAt.Unix()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// clientForPhone returns the connected account whose token can access the given
// WhatsApp phone number id, so a reply goes out from the account that actually
// owns that number (multi-account). Falls back to the first client.
func (h *MetaHandler) clientForPhone(phoneNumberID string) (metaClient, bool) {
	cs := h.clients()
	for _, mc := range cs {
		if _, err := mc.graph("/"+phoneNumberID, map[string]string{"fields": "id"}); err == nil {
			return mc, true
		}
	}
	if len(cs) > 0 {
		return cs[0], true
	}
	return metaClient{}, false
}

// graph performs an authenticated GET against the Graph API.
func (mc metaClient) graph(path string, params map[string]string) (map[string]any, error) {
	u, _ := url.Parse(mc.baseURL(path))
	q := u.Query()
	q.Set("access_token", mc.token)
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	res, err := mc.http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	// Graph reports auth/permission/quota failures in the body (often with a 200),
	// not as a transport error. Surface it instead of returning an empty result —
	// otherwise a "(#100) ads_read required" or expired token silently looks like
	// an account with zero ad accounts ("tidak ada akun iklan").
	if e, ok := out["error"].(map[string]any); ok {
		// Prefer Meta's human-facing message (error_user_msg) — it explains the
		// real cause (e.g. "minta akses lanjutan ke izin instagram_manage_messages")
		// far better than the terse internal "message" field.
		msg := gstr(e, "error_user_msg")
		if msg == "" {
			msg = gstr(e, "message")
		}
		if msg == "" {
			msg = "Graph API error"
		}
		if code := gnum(e, "code"); code != 0 {
			msg = fmt.Sprintf("%s (#%d)", msg, int(code))
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("Graph API HTTP %d", res.StatusCode)
	}
	return out, nil
}

func dataList(m map[string]any) []any {
	if m == nil {
		return nil
	}
	if d, ok := m["data"].([]any); ok {
		return d
	}
	return nil
}
func gstr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// gnum reads a numeric field that Graph may return as a string or a number.
func gnum(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	}
	return 0
}

func spendOf(a map[string]any) float64 {
	switch v := a["amount_spent"].(type) {
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	}
	return 0
}

// pickAccount returns the account to detail: the pinned id when set (matched
// with or without the act_ prefix), otherwise the highest-spend account so an
// empty/test account is never the default. Returns nil when a pinned id is set
// but not present in the list (caller then reads it directly).
func pickAccount(list []any, pinned string) map[string]any {
	pin := pinned
	if pin != "" && (len(pin) < 4 || pin[:4] != "act_") {
		pin = "act_" + pin
	}
	var best map[string]any
	bestSpend := -1.0
	for _, it := range list {
		a, _ := it.(map[string]any)
		if a == nil {
			continue
		}
		if pin != "" && gstr(a, "id") == pin {
			return a
		}
		if s := spendOf(a); s > bestSpend {
			bestSpend, best = s, a
		}
	}
	if pinned != "" {
		return nil
	}
	return best
}

// Ads — the most complete pull in one call: every accessible ad account with
// its 30-day summary, plus a full per-campaign breakdown (spend / result /
// cost-per-result / CTR / CPC) across all accounts, results parsed from the
// Meta `actions` field.
func (h *MetaHandler) Ads(c *gin.Context) {
	clients := h.clients()
	if len(clients) == 0 {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	preset := rangePreset(c)
	ckey := "ads:" + preset + ":" + h.connSig()
	if b, ok := h.getCache(ckey, 90*time.Second); ok {
		c.JSON(http.StatusOK, b)
		return
	}

	allAccounts := []any{}
	allCampaigns := []gin.H{}
	seen := map[string]bool{} // dedupe ad accounts across tokens so spend isn't double-counted
	var totSpend, totResults, totImpr, totClicks, totReach float64
	var totActive, totDelivering, totIssues int
	var firstErr string
	insFields := "spend,impressions,reach,frequency,clicks,ctr,cpc,cpm,actions"

	// Aggregate across EVERY connected account (each token), not just the active
	// one. An ad account visible to more than one token is counted once.
	for _, mc := range clients {
		acc, err := mc.graph("/me/adaccounts", map[string]string{"fields": "id,name,account_status,currency,amount_spent,balance", "limit": "100"})
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		for _, it := range dataList(acc) {
			a, _ := it.(map[string]any)
			if a == nil {
				continue
			}
			id := gstr(a, "id")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			// Friendly name from the connection label that surfaced this account
			// (Meta's own account name is often empty). Frontend: name || connLabel || id.
			if mc.label != "" {
				a["connLabel"] = mc.label
			}
			// Account summary (selected range) attached to each account.
			if ins, e := mc.graph("/"+id+"/insights", map[string]string{"date_preset": preset, "fields": "spend,impressions,clicks,ctr,reach"}); e == nil {
				if d := dataList(ins); len(d) > 0 {
					a["insights"] = d[0]
				}
			}
			allAccounts = append(allAccounts, a)
			// Campaign meta (status/objective) keyed by id.
			meta := map[string]map[string]any{}
			if cm, e := mc.graph("/"+id+"/campaigns", map[string]string{"fields": "id,name,status,effective_status,objective,daily_budget,issues_info", "limit": "500"}); e == nil {
				for _, ci := range dataList(cm) {
					if cmap, ok := ci.(map[string]any); ok {
						meta[gstr(cmap, "id")] = cmap
					}
				}
			}
			// Per-campaign insights for the selected range.
			ci, e := mc.graph("/"+id+"/insights", map[string]string{
				"level": "campaign", "date_preset": preset,
				"fields": "campaign_id,campaign_name," + insFields, "limit": "500",
			})
			if e != nil {
				continue
			}
			for _, row := range dataList(ci) {
				r, _ := row.(map[string]any)
				if r == nil {
					continue
				}
				cid := gstr(r, "campaign_id")
				label, results := resultFromActions(r)
				spend := gnum(r, "spend")
				impr := gnum(r, "impressions")
				clicks := gnum(r, "clicks")
				reach := gnum(r, "reach")
				cpr := 0.0
				if results > 0 {
					cpr = spend / results
				}
				m := meta[cid]
				status := mstr(m, "status")
				effStatus := mstr(m, "effective_status")
				issueCount, issueSummary := issuesOf(m)
				acctDisplay := gstr(a, "name")
				if acctDisplay == "" {
					acctDisplay = gstr(a, "connLabel")
				}
				if acctDisplay == "" {
					acctDisplay = strings.TrimPrefix(id, "act_")
				}
				allCampaigns = append(allCampaigns, gin.H{
					"id": cid, "name": gstr(r, "campaign_name"),
					"account":         acctDisplay,
					"accountId":       strings.TrimPrefix(id, "act_"),
					"status":          status,
					"effectiveStatus": effStatus,
					"issues":          issueCount,
					"issueSummary":    issueSummary,
					"objective":       strings.TrimPrefix(mstr(m, "objective"), "OUTCOME_"),
					"spend":           spend, "impressions": impr, "clicks": clicks,
					"reach": reach, "frequency": gnum(r, "frequency"),
					"ctr": gnum(r, "ctr"), "cpc": gnum(r, "cpc"), "cpm": gnum(r, "cpm"),
					"resultLabel": label, "results": results, "costPerResult": cpr,
				})
				totSpend += spend
				totResults += results
				totImpr += impr
				totClicks += clicks
				totReach += reach
				if status == "ACTIVE" {
					totActive++
					if spend > 0 {
						totDelivering++
					}
				}
				if issueCount > 0 {
					totIssues++
				}
			}
		}
	}

	// Only surface an error when nothing at all came back (one dead token among
	// several shouldn't blank the whole dashboard).
	if len(allAccounts) == 0 && firstErr != "" {
		c.JSON(http.StatusBadGateway, gin.H{"configured": true, "error": firstErr})
		return
	}

	// Sort campaigns by spend desc.
	sort.Slice(allCampaigns, func(i, j int) bool {
		return allCampaigns[i]["spend"].(float64) > allCampaigns[j]["spend"].(float64)
	})

	cpr := 0.0
	if totResults > 0 {
		cpr = totSpend / totResults
	}
	ctr := 0.0
	if totImpr > 0 {
		ctr = totClicks / totImpr * 100
	}
	cpc := 0.0
	if totClicks > 0 {
		cpc = totSpend / totClicks
	}
	cpm := 0.0
	if totImpr > 0 {
		cpm = totSpend / totImpr * 1000
	}
	freq := 0.0
	if totReach > 0 {
		freq = totImpr / totReach
	}
	cvr := 0.0 // conversion rate: hasil ÷ klik
	if totClicks > 0 {
		cvr = totResults / totClicks * 100
	}
	result := gin.H{
		"configured": true,
		"range":      preset,
		"accounts":   allAccounts,
		"campaigns":  allCampaigns,
		"totals": gin.H{
			"spend": totSpend, "results": totResults, "costPerResult": cpr,
			"impressions": totImpr, "clicks": totClicks, "ctr": ctr, "cpc": cpc, "cpm": cpm,
			"reach": totReach, "frequency": freq, "conversionRate": cvr,
			"campaigns": len(allCampaigns), "activeCampaigns": totActive,
			"deliveringCampaigns": totDelivering, "issueCampaigns": totIssues,
			"accounts": len(allAccounts),
		},
	}
	h.setCache(ckey, result)
	c.JSON(http.StatusOK, result)
}

// primaryAccountID resolves the ad account to break down: the pinned one, else
// the highest-spend account the token can see.
func (mc metaClient) primaryAccountID() string {
	if mc.adAccount != "" {
		if strings.HasPrefix(mc.adAccount, "act_") {
			return mc.adAccount
		}
		return "act_" + mc.adAccount
	}
	acc, _ := mc.graph("/me/adaccounts", map[string]string{"fields": "id,amount_spent", "limit": "100"})
	if a := pickAccount(dataList(acc), ""); a != nil {
		return gstr(a, "id")
	}
	return ""
}

// insightRows fetches insights rows with the standard metric fields + actions.
func (mc metaClient) insightRows(act string, params map[string]string) []map[string]any {
	base := map[string]string{"date_preset": "last_30d", "limit": "500", "fields": "spend,impressions,clicks,ctr,actions"}
	for k, v := range params {
		base[k] = v
	}
	r, e := mc.graph("/"+act+"/insights", base)
	if e != nil {
		return nil
	}
	out := []map[string]any{}
	for _, it := range dataList(r) {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// mapBreakdown turns insight rows into compact {label, spend, results, ...} and
// sorts by spend desc, keeping the top `limit` (0 = all).
func mapBreakdown(rows []map[string]any, label func(map[string]any) string, limit int) []gin.H {
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		_, res := resultFromActions(r)
		out = append(out, gin.H{
			"label": label(r), "spend": gnum(r, "spend"), "impressions": gnum(r, "impressions"),
			"clicks": gnum(r, "clicks"), "ctr": gnum(r, "ctr"), "results": res,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["spend"].(float64) > out[j]["spend"].(float64) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// AdsDetail — deep breakdowns for the primary account: daily trend, demographics
// (age/gender), placement, region, device, and top ads.
// bdAgg accumulates a breakdown segment's metrics while merging the same label
// across multiple ad accounts.
type bdAgg struct {
	spend, impressions, clicks, results float64
}

// bdSorted turns an aggregation map into the {label,spend,...} rows the UI
// expects, sorted by spend desc, keeping the top `limit` (0 = all).
func bdSorted(m map[string]*bdAgg, limit int) []gin.H {
	out := make([]gin.H, 0, len(m))
	for k, a := range m {
		ctr := 0.0
		if a.impressions > 0 {
			ctr = a.clicks / a.impressions * 100
		}
		out = append(out, gin.H{
			"label": k, "spend": a.spend, "impressions": a.impressions,
			"clicks": a.clicks, "ctr": ctr, "results": a.results,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["spend"].(float64) > out[j]["spend"].(float64) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// dailySorted returns daily rows in chronological order.
func dailySorted(m map[string]*bdAgg) []gin.H {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		out = append(out, gin.H{"date": k, "spend": a.spend, "results": a.results, "clicks": a.clicks, "impressions": a.impressions})
	}
	return out
}

// AdsDetail — deep breakdowns aggregated across EVERY connected ad account:
// daily trend, demographics (age/gender), placement, region, device, top ads.
// Each segment label is summed across accounts so the cards reflect the whole
// portfolio, not a single account.
func (h *MetaHandler) AdsDetail(c *gin.Context) {
	clients := h.clients()
	if len(clients) == 0 {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	preset := rangePreset(c)
	ckey := "detail:" + preset + ":" + h.connSig()
	if b, ok := h.getCache(ckey, 90*time.Second); ok {
		c.JSON(http.StatusOK, b)
		return
	}

	// Unique ad accounts across every token, paired with a client that reads it.
	type acctRef struct {
		id string
		mc metaClient
	}
	var accts []acctRef
	seen := map[string]bool{}
	var firstErr string
	for _, mc := range clients {
		acc, err := mc.graph("/me/adaccounts", map[string]string{"fields": "id", "limit": "100"})
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		for _, it := range dataList(acc) {
			a, _ := it.(map[string]any)
			if a == nil {
				continue
			}
			id := gstr(a, "id")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			accts = append(accts, acctRef{id: id, mc: mc})
		}
	}
	if len(accts) == 0 {
		if firstErr != "" {
			c.JSON(http.StatusBadGateway, gin.H{"configured": true, "error": firstErr})
		} else {
			c.JSON(http.StatusOK, gin.H{"configured": true, "error": "tidak ada akun iklan"})
		}
		return
	}

	daily := map[string]*bdAgg{}
	demo := map[string]*bdAgg{}
	place := map[string]*bdAgg{}
	region := map[string]*bdAgg{}
	device := map[string]*bdAgg{}
	topAds := map[string]*bdAgg{}
	hourly := map[string]*bdAgg{}
	// Creative winner: aggregated by ad copy (body) joined with live ad metrics.
	creatives := map[string]*creativeAgg{}
	add := func(m map[string]*bdAgg, key string, r map[string]any) {
		if key == "" {
			return
		}
		a := m[key]
		if a == nil {
			a = &bdAgg{}
			m[key] = a
		}
		_, res := resultFromActions(r)
		a.spend += gnum(r, "spend")
		a.impressions += gnum(r, "impressions")
		a.clicks += gnum(r, "clicks")
		a.results += res
	}

	for _, ar := range accts {
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "time_increment": "1"}) {
			add(daily, gstr(r, "date_start"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "breakdowns": "age,gender"}) {
			add(demo, gstr(r, "age")+" · "+gstr(r, "gender"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "breakdowns": "publisher_platform,platform_position"}) {
			add(place, gstr(r, "publisher_platform")+" · "+gstr(r, "platform_position"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "breakdowns": "region"}) {
			add(region, gstr(r, "region"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "breakdowns": "impression_device"}) {
			add(device, gstr(r, "impression_device"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "level": "ad", "fields": "ad_name,spend,impressions,clicks,ctr,actions"}) {
			add(topAds, gstr(r, "ad_name"), r)
		}
		for _, r := range ar.mc.insightRows(ar.id, map[string]string{"date_preset": preset, "breakdowns": "hourly_stats_aggregated_by_advertiser_time_zone"}) {
			add(hourly, gstr(r, "hourly_stats_aggregated_by_advertiser_time_zone"), r)
		}
		// Creative winner: standard ads have no asset-feed breakdown, so join
		// ad-level metrics with each ad's actual creative (body/cta/thumbnail),
		// aggregated by ad copy so the same winning text isn't repeated.
		ar.mc.collectCreatives(ar.id, preset, creatives)
	}

	result := gin.H{
		"configured": true, "accounts": len(accts),
		"daily": dailySorted(daily), "demographics": bdSorted(demo, 12), "placements": bdSorted(place, 12),
		"regions": bdSorted(region, 10), "devices": bdSorted(device, 10), "topAds": bdSorted(topAds, 12),
		"hourly": hourlySorted(hourly), "creatives": creativesSorted(creatives, 12),
	}
	h.setCache(ckey, result)
	c.JSON(http.StatusOK, result)
}

// creativeAgg accumulates one ad-copy's performance across the ads that use it.
type creativeAgg struct {
	body, title, cta, thumb, resultLabel string
	spend, results, impressions, clicks  float64
	ads                                  int
}

// collectCreatives joins live ad-level metrics with each ad's creative content
// and folds them into `out`, keyed by ad copy (body → title → name fallback).
// To stay fast it only resolves creatives for the top-spending ads (batched by
// id) instead of pulling every ad's creative.
func (mc metaClient) collectCreatives(act, preset string, out map[string]*creativeAgg) {
	metrics := map[string]map[string]any{}
	for _, r := range mc.insightRows(act, map[string]string{"date_preset": preset, "level": "ad", "fields": "ad_id,ad_name,spend,impressions,clicks,actions"}) {
		if aid := gstr(r, "ad_id"); aid != "" && gnum(r, "spend") > 0 {
			metrics[aid] = r
		}
	}
	if len(metrics) == 0 {
		return
	}
	// Top spenders only — keep the batch within Graph's 50-id limit.
	ids := make([]string, 0, len(metrics))
	for id := range metrics {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return gnum(metrics[ids[i]], "spend") > gnum(metrics[ids[j]], "spend") })
	if len(ids) > 40 {
		ids = ids[:40]
	}
	batch, err := mc.graph("", map[string]string{"ids": strings.Join(ids, ","), "fields": "id,name,creative{id,body,title,call_to_action_type,thumbnail_url}"})
	if err != nil {
		return
	}
	for _, id := range ids {
		ad, _ := batch[id].(map[string]any)
		if ad == nil {
			continue
		}
		m := metrics[id]
		cr, _ := ad["creative"].(map[string]any)
		body := gstr(cr, "body")
		title := gstr(cr, "title")
		key := body
		if key == "" {
			key = title
		}
		if key == "" {
			key = gstr(ad, "name")
		}
		label, results := resultFromActions(m)
		a := out[key]
		if a == nil {
			a = &creativeAgg{body: body, title: title, cta: gstr(cr, "call_to_action_type"), thumb: gstr(cr, "thumbnail_url"), resultLabel: label}
			out[key] = a
		}
		if a.thumb == "" {
			a.thumb = gstr(cr, "thumbnail_url")
		}
		if a.resultLabel == "" {
			a.resultLabel = label
		}
		a.spend += gnum(m, "spend")
		a.results += results
		a.impressions += gnum(m, "impressions")
		a.clicks += gnum(m, "clicks")
		a.ads++
	}
}

// creativesSorted ranks winning ad-copies by results (then spend) desc.
func creativesSorted(m map[string]*creativeAgg, limit int) []gin.H {
	out := make([]gin.H, 0, len(m))
	for _, a := range m {
		cpr := 0.0
		if a.results > 0 {
			cpr = a.spend / a.results
		}
		ctr := 0.0
		if a.impressions > 0 {
			ctr = a.clicks / a.impressions * 100
		}
		out = append(out, gin.H{
			"body": a.body, "title": a.title, "cta": a.cta, "thumbnail": a.thumb,
			"resultLabel": a.resultLabel, "spend": a.spend, "results": a.results, "costPerResult": cpr,
			"impressions": a.impressions, "clicks": a.clicks, "ctr": ctr, "ads": a.ads,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["results"].(float64) != out[j]["results"].(float64) {
			return out[i]["results"].(float64) > out[j]["results"].(float64)
		}
		return out[i]["spend"].(float64) > out[j]["spend"].(float64)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// hourlySorted returns hourly breakdown rows in chronological order (00→23h).
func hourlySorted(m map[string]*bdAgg) []gin.H {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		ctr := 0.0
		if a.impressions > 0 {
			ctr = a.clicks / a.impressions * 100
		}
		out = append(out, gin.H{"label": k, "spend": a.spend, "impressions": a.impressions, "clicks": a.clicks, "ctr": ctr, "results": a.results})
	}
	return out
}

// AdsCampaign — deep drill-down for ONE campaign (clickable from the table):
// headline metrics, daily trend, per-ad breakdown, and audience segments
// (age/gender, placement) for the selected range. The owning token is found by
// probing each connected client until the campaign node resolves.
func (h *MetaHandler) AdsCampaign(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id wajib diisi"})
		return
	}
	preset := rangePreset(c)
	clients := h.clients()
	if len(clients) == 0 {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}

	insFields := "spend,impressions,reach,frequency,clicks,ctr,cpc,cpm,actions"
	var firstErr string
	for _, mc := range clients {
		meta, err := mc.graph("/"+id, map[string]string{"fields": "id,name,status,objective,daily_budget,lifetime_budget,start_time,stop_time,account_id"})
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if gstr(meta, "id") == "" {
			continue // this token can't see the campaign — try the next
		}

		// Headline metrics for the range (single campaign-level row).
		var totals gin.H
		if rows := mc.insightRows(id, map[string]string{"date_preset": preset, "fields": insFields}); len(rows) > 0 {
			r := rows[0]
			label, results := resultFromActions(r)
			spend := gnum(r, "spend")
			cpr := 0.0
			if results > 0 {
				cpr = spend / results
			}
			totals = gin.H{
				"spend": spend, "impressions": gnum(r, "impressions"), "reach": gnum(r, "reach"),
				"frequency": gnum(r, "frequency"), "clicks": gnum(r, "clicks"), "ctr": gnum(r, "ctr"),
				"cpc": gnum(r, "cpc"), "cpm": gnum(r, "cpm"),
				"resultLabel": label, "results": results, "costPerResult": cpr,
			}
		}

		daily := map[string]*bdAgg{}
		demo := map[string]*bdAgg{}
		place := map[string]*bdAgg{}
		ads := map[string]*bdAgg{}
		adsets := map[string]*bdAgg{}
		add := func(m map[string]*bdAgg, key string, r map[string]any) {
			if key == "" {
				return
			}
			a := m[key]
			if a == nil {
				a = &bdAgg{}
				m[key] = a
			}
			_, res := resultFromActions(r)
			a.spend += gnum(r, "spend")
			a.impressions += gnum(r, "impressions")
			a.clicks += gnum(r, "clicks")
			a.results += res
		}
		for _, r := range mc.insightRows(id, map[string]string{"date_preset": preset, "time_increment": "1"}) {
			add(daily, gstr(r, "date_start"), r)
		}
		for _, r := range mc.insightRows(id, map[string]string{"date_preset": preset, "breakdowns": "age,gender"}) {
			add(demo, gstr(r, "age")+" · "+gstr(r, "gender"), r)
		}
		for _, r := range mc.insightRows(id, map[string]string{"date_preset": preset, "breakdowns": "publisher_platform,platform_position"}) {
			add(place, gstr(r, "publisher_platform")+" · "+gstr(r, "platform_position"), r)
		}
		for _, r := range mc.insightRows(id, map[string]string{"date_preset": preset, "level": "ad", "fields": "ad_name,spend,impressions,clicks,ctr,actions"}) {
			add(ads, gstr(r, "ad_name"), r)
		}
		for _, r := range mc.insightRows(id, map[string]string{"date_preset": preset, "level": "adset", "fields": "adset_name,spend,impressions,clicks,ctr,actions"}) {
			add(adsets, gstr(r, "adset_name"), r)
		}

		c.JSON(http.StatusOK, gin.H{
			"configured": true,
			"range":      preset,
			"campaign": gin.H{
				"id": gstr(meta, "id"), "name": gstr(meta, "name"), "status": gstr(meta, "status"),
				"objective":      strings.TrimPrefix(gstr(meta, "objective"), "OUTCOME_"),
				"dailyBudget":    gnum(meta, "daily_budget"),
				"lifetimeBudget": gnum(meta, "lifetime_budget"),
				"startTime":      gstr(meta, "start_time"), "stopTime": gstr(meta, "stop_time"),
				"accountId": strings.TrimPrefix(gstr(meta, "account_id"), "act_"),
			},
			"totals":       totals,
			"daily":        dailySorted(daily),
			"demographics": bdSorted(demo, 12),
			"placements":   bdSorted(place, 12),
			"ads":          bdSorted(ads, 20),
			"adsets":       bdSorted(adsets, 20),
		})
		return
	}

	if firstErr != "" {
		c.JSON(http.StatusBadGateway, gin.H{"configured": true, "error": firstErr})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"configured": true, "error": "campaign tidak ditemukan"})
}

// issuesOf reads Graph `issues_info` (delivery problems) from a campaign node
// and returns the count + a short summary of the first issue.
func issuesOf(m map[string]any) (int, string) {
	if m == nil {
		return 0, ""
	}
	arr, _ := m["issues_info"].([]any)
	if len(arr) == 0 {
		return 0, ""
	}
	summary := ""
	if first, ok := arr[0].(map[string]any); ok {
		summary = gstr(first, "error_summary")
		if summary == "" {
			summary = gstr(first, "error_message")
		}
	}
	return len(arr), summary
}

// mstr reads a string field from a possibly-nil map.
func mstr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	return gstr(m, k)
}

// resultFromActions picks the most meaningful conversion from Meta's `actions`
// array and returns a human label + count (messaging > lead > purchase > click).
func resultFromActions(r map[string]any) (string, float64) {
	actions, _ := r["actions"].([]any)
	vals := map[string]float64{}
	for _, it := range actions {
		a, _ := it.(map[string]any)
		if a == nil {
			continue
		}
		vals[gstr(a, "action_type")] = gnum(a, "value")
	}
	pri := []struct{ key, label string }{
		{"onsite_conversion.messaging_conversation_started_7d", "Percakapan WA"},
		{"onsite_conversion.total_messaging_connection", "Pesan"},
		{"onsite_conversion.lead_grouped", "Lead"},
		{"lead", "Lead"},
		{"purchase", "Pembelian"},
		{"link_click", "Klik Link"},
	}
	for _, p := range pri {
		if v, ok := vals[p.key]; ok && v > 0 {
			return p.label, v
		}
	}
	return "", 0
}

// WhatsApp — WhatsApp Business Accounts under the business + their phone numbers.
func (h *MetaHandler) WhatsApp(c *gin.Context) {
	clients := h.clients()
	if len(clients) == 0 {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	wabas := []gin.H{}
	seen := map[string]bool{}
	var firstErr string
	for _, mc := range clients {
		bizIDs, derr := mc.discoverBusinessIDs()
		if derr != nil && len(bizIDs) == 0 && firstErr == "" {
			firstErr = derr.Error()
		}
		for _, bid := range bizIDs {
			// Owned + client (shared) WABAs across each token's businesses.
			for _, edge := range []string{"owned_whatsapp_business_accounts", "client_whatsapp_business_accounts"} {
				r, err := mc.graph("/"+bid+"/"+edge, map[string]string{"fields": "id,name,timezone_id,message_template_namespace"})
				if err != nil {
					if firstErr == "" {
						firstErr = err.Error()
					}
					continue
				}
				for _, it := range dataList(r) {
					w, _ := it.(map[string]any)
					id := gstr(w, "id")
					if id == "" || seen[id] {
						continue
					}
					seen[id] = true
					entry := gin.H{"id": id, "name": gstr(w, "name")}
					if ph, e := mc.graph("/"+id+"/phone_numbers", map[string]string{"fields": "display_phone_number,verified_name,quality_rating,code_verification_status,platform_type"}); e == nil {
						entry["phones"] = dataList(ph)
					}
					if tpl, e := mc.graph("/"+id+"/message_templates", map[string]string{"fields": "name,status,category", "limit": "100"}); e == nil {
						entry["templates"] = dataList(tpl)
					}
					wabas = append(wabas, entry)
				}
			}
		}
	}
	if len(wabas) == 0 && firstErr != "" {
		c.JSON(http.StatusBadGateway, gin.H{"configured": true, "error": firstErr})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "wabas": wabas})
}

// discoverBusinessIDs lists the businesses to scan for WhatsApp accounts for one
// token: the pinned/env business id (if any), businesses via /me/businesses, and
// the business behind each Page (/me/accounts?fields=business). The Page path is
// the only one that resolves a System User token (its /me/businesses is empty).
func (mc metaClient) discoverBusinessIDs() ([]string, error) {
	ids := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	add(mc.businessID)

	var firstErr error
	if r, err := mc.graph("/me/businesses", map[string]string{"fields": "id", "limit": "100"}); err == nil {
		for _, it := range dataList(r) {
			if b, ok := it.(map[string]any); ok {
				add(gstr(b, "id"))
			}
		}
	} else {
		firstErr = err
	}
	if r, err := mc.graph("/me/accounts", map[string]string{"fields": "business", "limit": "100"}); err == nil {
		for _, it := range dataList(r) {
			if p, ok := it.(map[string]any); ok {
				if bz, ok := p["business"].(map[string]any); ok {
					add(gstr(bz, "id"))
				}
			}
		}
	} else if firstErr == nil {
		firstErr = err
	}

	if len(ids) == 0 {
		return ids, firstErr
	}
	return ids, nil
}

// Instagram — the IG professional accounts connected via the Instagram API with
// Instagram Login (each pasted as its own user token, stored in the DB). Returns
// configured=true whenever the store is up so the dashboard can show the
// "connect account" form even before any account is added.
func (h *MetaHandler) Instagram(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	igs := []any{}
	for _, a := range h.igAccounts() {
		igs = append(igs, gin.H{
			"connId":              a.ID,
			"id":                  a.IGUserID,
			"username":            a.Username,
			"followers_count":     a.Followers,
			"media_count":         a.MediaCount,
			"profile_picture_url": a.ProfilePicture,
			"page":                a.Name,
		})
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "instagram": igs})
}

// igAccounts returns every connected IG-login account (empty on store/DB error).
func (h *MetaHandler) igAccounts() []store.IGAccount {
	if h.wa == nil {
		return nil
	}
	accs, err := h.wa.ListIGAccounts()
	if err != nil {
		return nil
	}
	return accs
}

// findIGAccount resolves a connected account by its IG user id (the value the
// frontend sends back as page_id).
func (h *MetaHandler) findIGAccount(igUserID string) *store.IGAccount {
	if h.wa == nil {
		return nil
	}
	a, err := h.wa.FindIGAccount(igUserID)
	if err != nil {
		return nil
	}
	return a
}

// igClient builds a client for the Instagram API with Instagram Login
// (graph.instagram.com), bound to one account's user access token.
func (h *MetaHandler) igClient(token string, httpc *http.Client) metaClient {
	if httpc == nil {
		httpc = h.http
	}
	return metaClient{token: token, ver: h.ver, host: "https://graph.instagram.com", http: httpc}
}

// igSig is the IG conversation cache-key fragment — changes when the connected
// account set changes, so connect/disconnect busts the cache.
func (h *MetaHandler) igSig() string {
	accs := h.igAccounts()
	if len(accs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(accs))
	for _, a := range accs {
		parts = append(parts, fmt.Sprintf("%d@%d", a.ID, a.UpdatedAt.Unix()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// ===================== INSTAGRAM INBOX (DM) =====================
//
// IG Direct messages are read/sent via the Instagram API with Instagram Login
// (graph.instagram.com) using each account's own user access token (pasted via
// the dashboard, stored in the DB, auto-refreshed). Reading the conversation
// list of an account that talks to non-app-role users requires Advanced Access
// to instagram_business_manage_messages — until that is granted Graph returns a
// timeout which we surface via the `limited` field so the UI can explain it.

// graphPost performs an authenticated POST with a JSON body (used to send DMs).
func (mc metaClient) graphPost(path string, body map[string]any) (map[string]any, error) {
	u, _ := url.Parse(mc.baseURL(path))
	q := u.Query()
	q.Set("access_token", mc.token)
	u.RawQuery = q.Encode()
	b, _ := json.Marshal(body)
	res, err := mc.http.Post(u.String(), "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	if e, ok := out["error"].(map[string]any); ok {
		msg := gstr(e, "error_user_msg")
		if msg == "" {
			msg = gstr(e, "message")
		}
		if msg == "" {
			msg = "Graph API error"
		}
		if code := gnum(e, "code"); code != 0 {
			msg = fmt.Sprintf("%s (#%d)", msg, int(code))
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// sanitizeIGError turns a raw conversations-fetch error into a safe, friendly
// message. A transport timeout's error string contains the request URL with the
// page access token embedded — never expose that. Meta's own error_user_msg
// (which mentions the permission) is clean and kept as-is.
func sanitizeIGError(raw string) string {
	if strings.Contains(raw, "instagram_manage_messages") || strings.Contains(raw, "akses lanjutan") {
		return raw // Meta's human-facing explanation — safe, already descriptive
	}
	if strings.Contains(raw, "access_token") || strings.Contains(raw, "graph.facebook.com") ||
		strings.Contains(raw, "deadline exceeded") || strings.Contains(raw, "Timeout") {
		return "Daftar percakapan tidak dapat dimuat — Meta memblokir sampai izin instagram_manage_messages mendapat Advanced Access (permintaan ke Meta melebihi batas waktu)."
	}
	return raw
}

// IGConversations — list IG DM threads across every connected IG-login account.
// Each thread carries the customer's name + IGSID (recipient for replies), last
// snippet, and unread count. Cached 60s; busted on webhook bump / connect.
func (h *MetaHandler) IGConversations(c *gin.Context) {
	if h.wa == nil {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	if len(h.igAccounts()) == 0 {
		c.JSON(http.StatusOK, gin.H{"configured": true, "conversations": []any{}, "accounts": 0})
		return
	}
	igKey := "ig:conversations:" + h.igSig()
	if b, ok := h.getCache(igKey, 60*time.Second); ok {
		c.JSON(http.StatusOK, b)
		return
	}
	out := h.igFetchConversations()
	h.setCache(igKey, out)
	c.JSON(http.StatusOK, out)
}

// igFetchConversations fans out across every connected account, querying the
// Instagram-login conversations endpoint (graph.instagram.com/me/conversations)
// with each account's own token. Returns the assembled body (no cache).
func (h *MetaHandler) igFetchConversations() gin.H {
	accs := h.igAccounts()
	fastHTTP := &http.Client{Timeout: 15 * time.Second}
	type accResult struct {
		convs   []gin.H
		limited string
	}
	results := make([]accResult, len(accs))
	var wg sync.WaitGroup
	for i, a := range accs {
		wg.Add(1)
		go func(i int, a store.IGAccount) {
			defer wg.Done()
			mc := h.igClient(a.AccessToken, fastHTTP)
			res, err := mc.graph("/me/conversations", map[string]string{
				"platform": "instagram",
				"fields":   "id,updated_time,unread_count,participants,messages.limit(1){message,from,created_time}",
				"limit":    "50",
			})
			if err != nil {
				results[i] = accResult{limited: err.Error()}
				return
			}
			var convs []gin.H
			for _, it := range dataList(res) {
				cm, _ := it.(map[string]any)
				if cm == nil {
					continue
				}
				// Customer = the participant that is not our own IG account.
				custName, custID := "", ""
				if parts, ok := cm["participants"].(map[string]any); ok {
					for _, pit := range dataList(parts) {
						pp, _ := pit.(map[string]any)
						if pp == nil {
							continue
						}
						id := gstr(pp, "id")
						user := gstr(pp, "username")
						if id == a.IGUserID || (user != "" && user == a.Username) {
							continue
						}
						custID = id
						custName = user
						if custName == "" {
							custName = gstr(pp, "name")
						}
					}
				}
				// Last message snippet.
				snippet := ""
				if msgs, ok := cm["messages"].(map[string]any); ok {
					if d := dataList(msgs); len(d) > 0 {
						if m0, ok := d[0].(map[string]any); ok {
							snippet = gstr(m0, "message")
						}
					}
				}
				convs = append(convs, gin.H{
					"id":          gstr(cm, "id"),
					"pageId":      a.IGUserID, // carries the IG account id back for thread/send
					"igUser":      a.Username,
					"customer":    custName,
					"recipientId": custID,
					"snippet":     snippet,
					"updatedTime": gstr(cm, "updated_time"),
					"unread":      gnum(cm, "unread_count"),
				})
			}
			results[i] = accResult{convs: convs}
		}(i, a)
	}
	wg.Wait()

	convs := []gin.H{}
	var limited string
	for _, r := range results {
		convs = append(convs, r.convs...)
		if limited == "" && r.limited != "" {
			limited = sanitizeIGError(r.limited)
		}
	}
	sort.Slice(convs, func(i, j int) bool {
		return gstr(convs[i], "updatedTime") > gstr(convs[j], "updatedTime")
	})
	out := gin.H{"configured": true, "conversations": convs, "accounts": len(accs)}
	if len(convs) == 0 && limited != "" {
		out["limited"] = limited
	}
	return out
}

// IGMessages — full message history of one thread. `page_id` carries the IG
// account id; `fromMe` marks our own replies so the UI renders them on the right.
func (h *MetaHandler) IGMessages(c *gin.Context) {
	convID := c.Query("conversation_id")
	igID := c.Query("page_id")
	if convID == "" || igID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id & page_id wajib"})
		return
	}
	a := h.findIGAccount(igID)
	if a == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "akun IG tidak ditemukan / token belum ditambah"})
		return
	}
	mc := h.igClient(a.AccessToken, nil)
	res, err := mc.graph("/"+convID, map[string]string{
		"fields": "messages.limit(80){id,message,from,created_time}",
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	msgs := []gin.H{}
	if mm, ok := res["messages"].(map[string]any); ok {
		for _, it := range dataList(mm) {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			fromID, fromUser := "", ""
			if f, ok := m["from"].(map[string]any); ok {
				fromID = gstr(f, "id")
				fromUser = gstr(f, "username")
			}
			msgs = append(msgs, gin.H{
				"id":     gstr(m, "id"),
				"text":   gstr(m, "message"),
				"time":   gstr(m, "created_time"),
				"fromMe": fromID == a.IGUserID || (fromUser != "" && fromUser == a.Username),
				"fromId": fromID,
			})
		}
	}
	// Graph returns newest-first; reverse to chronological for chat display.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	c.JSON(http.StatusOK, gin.H{"messages": msgs, "igUser": a.Username})
}

// IGSend — reply to a thread via the Instagram-login send endpoint
// (graph.instagram.com/me/messages). `page_id` carries the IG account id.
func (h *MetaHandler) IGSend(c *gin.Context) {
	var req struct {
		PageID      string `json:"page_id"`
		RecipientID string `json:"recipient_id"`
		Text        string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PageID == "" || req.RecipientID == "" || strings.TrimSpace(req.Text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page_id, recipient_id, dan text wajib"})
		return
	}
	a := h.findIGAccount(req.PageID)
	if a == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "akun IG tidak ditemukan / token belum ditambah"})
		return
	}
	mc := h.igClient(a.AccessToken, nil)
	res, err := mc.graphPost("/me/messages", map[string]any{
		"recipient": map[string]string{"id": req.RecipientID},
		"message":   map[string]string{"text": req.Text},
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "result": res})
}
