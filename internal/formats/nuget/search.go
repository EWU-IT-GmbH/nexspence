package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
	"github.com/nexspence-oss/nexspence/internal/service"
)

const searchTimeout = 5 * time.Second
const maxSearchBytes = 4 << 20

type queryError struct {
	status int
	code   string
}

func (e *queryError) Error() string { return e.code }
func unavailable(code string) error { return &queryError{503, code} }
func invalid(code string) error     { return &queryError{400, code} }

type SearchOptions struct {
	Query               string
	Skip, Take          int
	Prerelease, SemVer2 bool
}

func parseSearch(raw string) (SearchOptions, error) {
	o := SearchOptions{Take: 20}
	if len(raw) > 8192 {
		return o, &queryError{414, "query_too_long"}
	}
	q, e := url.ParseQuery(raw)
	if e != nil {
		return o, invalid("invalid_parameter")
	}
	for _, k := range []string{"q", "skip", "take", "prerelease", "semVerLevel", "packageType"} {
		if len(q[k]) > 1 {
			return o, invalid("duplicate_parameter")
		}
	}
	text := q.Get("q")
	if !utf8.ValidString(text) || len(text) > 1024 || utf8.RuneCountInString(text) > 256 {
		return o, invalid("invalid_query")
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return o, invalid("invalid_query")
		}
	}
	o.Query = strings.TrimSpace(text)
	for _, p := range []struct {
		k        string
		dest     *int
		min, max int
	}{{"skip", &o.Skip, 0, 10000}, {"take", &o.Take, 1, 1000}} {
		if vals, ok := q[p.k]; ok {
			v := vals[0]
			if v == "" {
				return o, invalid("invalid_parameter")
			}
			for _, r := range v {
				if r < '0' || r > '9' {
					return o, invalid("invalid_parameter")
				}
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < p.min || n > p.max {
				return o, invalid("invalid_parameter")
			}
			*p.dest = n
		}
	}
	if v, ok := q["prerelease"]; ok {
		switch strings.ToLower(v[0]) {
		case "true":
			o.Prerelease = true
		case "false":
		default:
			return o, invalid("invalid_parameter")
		}
	}
	if v, ok := q["semVerLevel"]; ok {
		o.SemVer2, e = nugetmeta.SemVerLevel(v[0])
		if e != nil {
			return o, invalid("invalid_parameter")
		}
	}
	if q.Get("packageType") != "" {
		return o, invalid("unsupported_parameter")
	}
	return o, nil
}

// SearchScope explicitly binds a read to its caller-facing repository and source
// order. AP-02 supplies one hosted repository; groups can use the same service.
type SearchScope struct {
	Repository       *domain.Repository
	Members          []string
	Proxies          []string
	UnsupportedOrder bool
	Caller           string
	CanRead          func(string) bool // immutable policy snapshot, never nil
}

func (h *Handler) hostedScope(c *gin.Context, repo *domain.Repository) (SearchScope, error) {
	scope := SearchScope{Repository: repo, Members: []string{repo.Name}, Caller: callerCacheKey(c.GetString("userID"), c.GetStringSlice("roles"), repo)}
	if (repo.Type != domain.TypeHosted && repo.Type != domain.TypeGroup) || !repo.Online {
		return scope, unavailable("search_unavailable")
	}
	if repo.Type == domain.TypeGroup {
		scope.Members = []string{}
		seen := map[string]bool{}
		for _, name := range domain.GroupMemberNames(repo) {
			if seen[name] {
				continue
			}
			seen[name] = true
			member, err := h.deps.Repos.Get(c.Request.Context(), name)
			if err != nil || member == nil {
				return scope, unavailable("search_unavailable")
			}
			if !member.Online || member.Format != repo.Format || member.Type == domain.TypeGroup {
				continue
			}
			switch member.Type {
			case domain.TypeHosted:
				if len(scope.Proxies) > 0 {
					scope.UnsupportedOrder = true
				}
				scope.Members = append(scope.Members, name)
			case domain.TypeProxy:
				scope.Proxies = append(scope.Proxies, name)
			default:
				return scope, unavailable("search_unavailable")
			}
		}
	}

	if v, ok := c.Get("tokenScopes"); ok {
		ss, _ := v.([]string)
		allowed := len(ss) == 0
		for _, s := range ss {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "read", "write", "delete":
				allowed = true
			}
		}
		if !allowed {
			return scope, &queryError{403, "access_denied"}
		}
	}
	if groupName := c.GetString("nugetCallerRepository"); groupName != "" && groupName != repo.Name {
		group, err := h.deps.Repos.Get(c.Request.Context(), groupName)
		if err != nil || group == nil || group.Type != domain.TypeGroup || group.Format != "nuget" {
			return scope, unavailable("search_unavailable")
		}
		member := false
		for _, name := range domain.GroupMemberNames(group) {
			if name == repo.Name {
				member = true
			}
		}
		if !member {
			return scope, unavailable("search_unavailable")
		}
		scope.Repository = group
		repo = group
	}
	snapshot, ok := h.deps.RBAC.(formats.ReadPolicySnapshotter)
	if !ok {
		return scope, unavailable("search_unavailable")
	}
	uid := c.GetString("userID")
	roles := c.GetStringSlice("roles")
	policy, e := snapshot.SnapshotReadPolicy(c.Request.Context(), uid, roles, repo)
	if c.Request.Context().Err() != nil {
		return scope, c.Request.Context().Err()
	}
	if e != nil || policy == nil {
		return scope, unavailable("search_unavailable")
	}
	if repo.Type == domain.TypeGroup && repo.RoutingRuleID != nil {
		if h.deps.RoutingRules == nil {
			return scope, unavailable("search_unavailable")
		}
		rule, err := h.deps.RoutingRules.Get(c.Request.Context(), *repo.RoutingRuleID)
		if err != nil || rule == nil {
			return scope, unavailable("search_unavailable")
		}
		scope.CanRead = func(path string) bool { return policy(path) && service.Allow(rule, path) }
	} else {
		scope.CanRead = policy
	}
	return scope, nil
}
func registrationRoot(sem2 bool) string {
	if sem2 {
		return "/v3/registration-semver2/"
	}
	return "/v3/registration/"
}
func packagePaths(id, key string, sem2 bool) []string {
	id = url.PathEscape(id)
	key = url.PathEscape(key)
	r := registrationRoot(sem2) + id
	return []string{"/v3/flatcontainer/" + id + "/index.json", "/v3/flatcontainer/" + id + "/" + key + "/" + id + "." + key + ".nupkg", r + "/index.json", r + "/" + key + ".json"}
}
func visible(scope SearchScope, c repository.NuGetCandidate, sem2 bool) bool {
	if scope.CanRead == nil {
		return false
	}
	for _, p := range packagePaths(c.ID, c.Key, sem2) {
		if !scope.CanRead(p) {
			return false
		}
	}
	return true
}
func canonicalPath(c repository.NuGetCandidate) string {
	return "/" + c.ID + "/" + c.Key + "/" + c.ID + "." + c.Key + ".nupkg"
}

// packageCandidates streams one ID at a time. It resolves aliases before version
// filtering so an unlisted priority winner cannot reveal a lower-priority copy.
func packageCandidates(ctx context.Context, s repository.NuGetSnapshot, scope SearchScope, q repository.NuGetQuery, sem2 bool, fn func(string, []repository.NuGetCandidate) error) error {
	priorities := map[string]int{}
	for i, n := range scope.Members {
		priorities[n] = i
	}
	current := ""
	winners := map[string]repository.NuGetCandidate{}
	bytes := 0
	flush := func() error {
		if current == "" {
			return nil
		}
		v := make([]repository.NuGetCandidate, 0, len(winners))
		for _, c := range winners {
			v = append(v, c)
		}
		return fn(current, v)
	}
	e := s.Walk(ctx, q, func(c repository.NuGetCandidate) error {
		if c.ID != current {
			if e := flush(); e != nil {
				return e
			}
			current = c.ID
			winners = map[string]repository.NuGetCandidate{}
			bytes = 0
		}
		if !visible(scope, c, sem2) {
			return nil
		}
		v, e := nugetmeta.ParseVersion(c.Version)
		if e != nil || !nugetmeta.ValidID(c.ID) || v.Key() != c.Key {
			return unavailable("search_unavailable")
		}
		if prior, ok := winners[c.Key]; ok {
			if priorities[c.Repository] > priorities[prior.Repository] {
				return nil
			}
			if c.Repository == prior.Repository {
				if prior.Path == canonicalPath(prior) {
					return nil
				}
				if c.Path != canonicalPath(c) {
					return unavailable("search_unavailable")
				}
			}
		} else {
			bytes += len(c.ID) + len(c.Version) + len(c.Path) + 256
			if bytes > maxSearchBytes {
				return unavailable("result_too_large")
			}
		}
		winners[c.Key] = c
		return nil
	})
	if e != nil {
		return e
	}
	return flush()
}

type SearchVersion struct {
	Version   string `json:"version"`
	Downloads int64  `json:"downloads"`
	ID        string `json:"@id"`
}
type SearchResult struct {
	ID           string          `json:"id"`
	Version      string          `json:"version"`
	Registration string          `json:"registration"`
	Versions     []SearchVersion `json:"versions"`
	Description  string          `json:"description,omitempty"`
	Authors      string          `json:"authors,omitempty"`
	Title        string          `json:"title,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	Tags         string          `json:"tags,omitempty"`
	LicenseURL   string          `json:"licenseUrl,omitempty"`
	ProjectURL   string          `json:"projectUrl,omitempty"`
	IconURL      string          `json:"iconUrl,omitempty"`
}
type SearchResponse struct {
	TotalHits int            `json:"totalHits"`
	Data      []SearchResult `json:"data"`
}

// Shared hosted ordering for standalone repositories and federated groups.
func hostedSearchTier(id, query string) int {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 3
	}
	if strings.EqualFold(id, query) {
		return 0
	}
	if strings.HasPrefix(strings.ToLower(id), query) {
		return 2
	}
	return 3
}

func (h *Handler) Search(ctx context.Context, scope SearchScope, o SearchOptions) (SearchResponse, error) {
	out := SearchResponse{Data: []SearchResult{}}
	if h.deps.NuGet == nil || scope.Repository == nil || scope.CanRead == nil {
		return out, unavailable("search_unavailable")
	}
	if !scope.CanRead("/v3/query") {
		return out, &queryError{403, "access_denied"}
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	e := h.deps.NuGet.Snapshot(ctx, func(s repository.NuGetSnapshot) error {
		ready, e := s.Ready(ctx, scope.Members)
		if e != nil {
			return e
		}
		if !ready {
			return unavailable("metadata_not_ready")
		}
		ids := []string{}
		selected := []string{}
		selectedBytes := 0
		e = packageCandidates(ctx, s, scope, repository.NuGetQuery{Repositories: scope.Members, Query: o.Query}, o.SemVer2, func(id string, cs []repository.NuGetCandidate) error {
			matches := []repository.NuGetCandidate{}
			for _, c := range cs {
				if c.Listed && (o.Prerelease || !c.Prerelease) && (o.SemVer2 || !c.SemVer2) {
					matches = append(matches, c)
				}
			}
			if len(matches) == 0 {
				return nil
			}
			ids = append(ids, id)
			selectedBytes += len(id)
			if selectedBytes > remoteCollectionBytes {
				return unavailable("result_too_large")
			}
			return nil
		})
		if e != nil {
			return e
		}
		sort.Slice(ids, func(i, j int) bool {
			a, b := hostedSearchTier(ids[i], o.Query), hostedSearchTier(ids[j], o.Query)
			if a != b {
				return a < b
			}
			return ids[i] < ids[j]
		})
		out.TotalHits = len(ids)
		if o.Skip >= len(ids) {
			return nil
		}
		ids = ids[o.Skip:min(len(ids), o.Skip+o.Take)]
		selectedIDs := map[string]bool{}
		for _, id := range ids {
			selectedIDs[id] = true
		}
		selectedBytes = 0
		// Reuse the same snapshot, loading version references only for the chosen IDs.
		e = packageCandidates(ctx, s, scope, repository.NuGetQuery{Repositories: scope.Members, Query: o.Query}, o.SemVer2, func(id string, cs []repository.NuGetCandidate) error {
			if !selectedIDs[id] {
				return nil
			}
			for _, c := range cs {
				if !c.Listed || (!o.Prerelease && c.Prerelease) || (!o.SemVer2 && c.SemVer2) {
					continue
				}
				selectedBytes += len(c.Version) + len(c.ID) + 256
				if selectedBytes > maxSearchBytes {
					return unavailable("result_too_large")
				}
				selected = append(selected, c.AssetID)
			}
			return nil
		})
		if e != nil {
			return e
		}
		records, e := s.Records(ctx, selected)
		if e != nil {
			return e
		}
		byID := map[string][]repository.NuGetRecord{}
		for _, r := range records {
			byID[r.Metadata.ID] = append(byID[r.Metadata.ID], r)
		}
		root := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(scope.Repository.Name)
		for _, id := range ids {
			rs := byID[id]
			if len(rs) == 0 {
				return unavailable("search_unavailable")
			}
			sort.Slice(rs, func(i, j int) bool {
				a, _ := nugetmeta.ParseVersion(rs[i].Metadata.Version)
				b, _ := nugetmeta.ParseVersion(rs[j].Metadata.Version)
				return a.Compare(b) < 0
			})
			m := rs[len(rs)-1].Metadata
			result := SearchResult{ID: id, Version: m.Version, Registration: root + registrationRoot(o.SemVer2) + url.PathEscape(id) + "/index.json", Versions: []SearchVersion{}, Description: m.Description, Authors: m.Authors, Title: m.Title, Summary: m.Summary, Tags: m.Tags, LicenseURL: m.LicenseURL, ProjectURL: m.ProjectURL, IconURL: m.IconURL}
			for _, r := range rs {
				result.Versions = append(result.Versions, SearchVersion{Version: r.Metadata.Version, Downloads: r.Asset.DownloadCount, ID: root + registrationRoot(o.SemVer2) + url.PathEscape(id) + "/" + url.PathEscape(r.Metadata.Key) + ".json"})
			}
			out.Data = append(out.Data, result)
		}
		return nil
	})
	if ctx.Err() != nil {
		return out, unavailable("search_timeout")
	}
	return out, e
}
func (h *Handler) serveSearch(c *gin.Context, repo *domain.Repository) {
	c.Header("Cache-Control", "private, no-store")
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Header("Allow", "GET, HEAD")
		writeQueryError(c, &queryError{405, "method_not_allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), searchTimeout)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	o, e := parseSearch(c.Request.URL.RawQuery)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	scope, e := h.hostedScope(c, repo)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	if scope.UnsupportedOrder {
		writeQueryError(c, unavailable("unsupported_member_order"))
		return
	}
	var result SearchResponse
	if len(scope.Proxies) > 0 {
		result, e = h.federatedSearch(ctx, scope, o)
	} else {
		result, e = h.Search(ctx, scope, o)
	}
	if e != nil {
		writeQueryError(c, e)
		return
	}
	if len(scope.Proxies) > 0 {
		// A normal Rider page of popular packages can contain tens of thousands
		// of version entries. Keep it bounded without applying the small-document
		// limit used for individual registration and hosted metadata documents.
		writeNuGetJSONLimit(c, result, false, remoteCollectionBytes)
	} else {
		writeNuGetJSON(c, result, false)
	}
}
func writeQueryError(c *gin.Context, e error) {
	status, code := 503, "search_unavailable"
	var q *queryError
	if errors.As(e, &q) {
		status, code = q.status, q.code
	}
	if errors.Is(e, repository.ErrNuGetResultTooLarge) {
		status, code = 503, "result_too_large"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		status, code = 503, "search_timeout"
	}
	if status == 403 && c.GetString("userID") == "" {
		status = 401
		c.Header("WWW-Authenticate", `Basic realm="Nexspence"`)
	}
	b, _ := json.Marshal(gin.H{"error": code})
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	if c.Request.Method != http.MethodHead {
		_, _ = c.Writer.Write(b)
	}
}
func writeNuGetJSON(c *gin.Context, v any, gzipBody bool) {
	writeNuGetJSONLimit(c, v, gzipBody, maxSearchBytes)
}

func writeNuGetJSONLimit(c *gin.Context, v any, gzipBody bool, limit int) {
	if e := c.Request.Context().Err(); e != nil {
		writeQueryError(c, e)
		return
	}
	b, e := json.Marshal(v)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	if e := c.Request.Context().Err(); e != nil {
		writeQueryError(c, e)
		return
	}
	if len(b) > limit {
		writeQueryError(c, unavailable("result_too_large"))
		return
	}
	if gzipBody {
		b, e = gzipJSON(b)
		if e != nil {
			writeQueryError(c, e)
			return
		}
		c.Header("Content-Encoding", "gzip")
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Length", fmt.Sprint(len(b)))
	c.Status(http.StatusOK)
	if c.Request.Method != http.MethodHead {
		_, _ = c.Writer.Write(b)
	}
}
