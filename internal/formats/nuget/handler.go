// Package nuget implements the NuGet v2/v3 repository protocol.
//
// NuGet v3 endpoints (under /repository/:repoName/):
//
//	GET  /index.json                        → service index (v3)
//	GET  /v3/registration/:id/index.json    → package registration (metadata)
//	GET  /v3/flatcontainer/:id/index.json   → version list
//	GET  /v3/flatcontainer/:id/:ver/:id.:ver.nupkg → download
//
// NuGet v2 endpoints:
//
//	GET  /FindPackagesById()?id='name'      → OData XML
//	PUT  /v2/package                        → nuget push (multipart)
//	DELETE /v2/packages/:id/:ver            → delete
package nuget

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/base"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

// Handler serves the NuGet v2/v3 repository protocol.
type Handler struct {
	deps   formats.Deps
	remote *remoteCache
}

// New creates a NuGet format Handler with the given dependencies.
func New(deps formats.Deps) *Handler { return &Handler{deps: deps, remote: newRemoteCache()} }

// Name returns the format identifier.
func (h *Handler) Name() string { return "nuget" }

func (h *Handler) ServeHTTP(c *gin.Context) {
	p := normPath(c.Param("path"))
	repoName := c.Param("repoName")

	repo, _ := h.deps.Repos.Get(c.Request.Context(), repoName)

	// Proxy: block mutations; rewrite service index; cache packages.
	if repo != nil && repo.Type == domain.TypeProxy {
		if repoproxy.RejectMutation(c, repo) {
			return
		}
		if (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) && p == "/index.json" {
			h.fetchAndRewriteNuGetIndex(c, repo)
			return
		}
		if p == "/v3/query" || p == "/query" {
			h.serveProxySearch(c, repo)
			return
		}
		// .nupkg package content is immutable; registration/flat-container index
		// pages are mutable metadata (new versions appear) and revalidate on a TTL.
		var maxAge time.Duration
		if !strings.HasSuffix(p, ".nupkg") {
			maxAge = repoproxy.MetadataMaxAge(repo)
		}
		coords := proxyCoords(p)
		// Resource paths advertised by the rewritten index are the upstream's
		// own paths re-rooted locally — forward them onto the bare origin,
		// not onto a possibly /v3-suffixed remote_url (#349).
		upstreamPath := ""
		if origin := nugetRemoteOrigin(remoteURLOf(repo)); origin != "" {
			upstreamPath = origin + p
		}
		if c.GetString("nugetCallerRepository") != "" && (strings.HasPrefix(p, "/v3/flatcontainer/") || strings.HasPrefix(p, registrationRoot(false)) || strings.HasPrefix(p, registrationRoot(true))) {
			cached := false
			if strings.HasSuffix(p, ".nupkg") {
				a, err := h.deps.Assets.GetByPath(c.Request.Context(), repo.Name, p)
				cached = err == nil && a != nil
			}
			if !cached {
				var err error
				upstreamPath, err = groupProxyPath(c.Request.Context(), repo, p)
				if err != nil {
					writeQueryError(c, err)
					return
				}
				// groupProxyPath derives the target from this member's service
				// index. Keep its auth scope local to this member attempt.
				request := c.Request
				c.Request = request.WithContext(repoproxy.WithTrustedAuthBases(request.Context(), upstreamPath))
				defer func() { c.Request = request }()
			}
		}
		// Registration pages embed absolute upstream URLs (packageContent,
		// @id) — rewrite them on serve so clients pull packages through this
		// proxy (#98); the cache keeps the upstream original.
		var rewrite func([]byte) []byte
		if strings.HasPrefix(p, "/v3/registration/") || strings.HasPrefix(p, "/v3/registration-semver2/") {
			caller := repo.Name
			if groupName := c.GetString("nugetCallerRepository"); groupName != "" {
				caller = groupName
			}
			localBase := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(caller)
			rewrite = func(b []byte) []byte { return RewriteRegistration(b, localBase) }
			if c.GetString("nugetCallerRepository") != "" {
				rewrite = func(b []byte) []byte {
					sem2 := strings.HasPrefix(p, registrationRoot(true))
					remoteRoot := strings.TrimSuffix(upstreamPath, strings.TrimPrefix(p, registrationRoot(sem2)))
					b = rewriteGroupRegistration(b, localBase, sem2, remoteRoot)
					if sem2 {
						compressed, err := gzipJSON(b)
						if err == nil {
							c.Header("Content-Encoding", "gzip")
							return compressed
						}
					}
					return b
				}
			}
		}
		if err := repoproxy.ServeGETRewritten(c, h.deps, repo, p, upstreamPath, coords, "application/octet-stream", maxAge, rewrite); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}

	if p == "/v3/query" {
		if repo == nil {
			writeQueryError(c, unavailable("search_unavailable"))
			return
		}
		h.serveSearch(c, repo)
		return
	}
	if repo != nil && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
		if strings.HasPrefix(p, "/v3/registration/") || strings.HasPrefix(p, "/v3/registration-semver2/") {
			h.serveHostedRegistration(c, repo, p)
			return
		}
		if strings.HasPrefix(p, "/v3/flatcontainer/") && strings.HasSuffix(p, "/index.json") {
			h.serveHostedVersions(c, repo, p)
			return
		}
		if strings.HasPrefix(p, "/v3/flatcontainer/") && strings.HasSuffix(p, ".nupkg") {
			h.serveHostedDownload(c, repo, p)
			return
		}
	}
	switch {
	// v3 service index
	case (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) && p == "/index.json":
		h.serveIndex(c, repoName)

	// v2 OData query: FindPackagesById()
	case c.Request.Method == http.MethodGet && strings.HasPrefix(p, "/FindPackagesById"):
		pkgID := c.Query("id")
		pkgID = strings.Trim(pkgID, "'")
		h.serveFindPackages(c, repoName, pkgID)

	// v2 push
	case c.Request.Method == http.MethodPut && p == "/v2/package":
		h.handlePush(c, repoName)

	// v2 delete: DELETE {PackagePublish}/:id/:ver. The service index
	// advertises PackagePublish as /v2/package, so that is what
	// `dotnet nuget delete` sends; /v2/packages/ stays for existing callers.
	case c.Request.Method == http.MethodDelete &&
		(strings.HasPrefix(p, "/v2/package/") || strings.HasPrefix(p, "/v2/packages/")):
		h.handleDelete(c, repoName, p)

	default:
		c.Status(http.StatusMethodNotAllowed)
	}
}

// handleDelete removes one package version (#589). The id and version are
// matched the way NuGet compares them, not as the user typed them. The version lists
// are built from components, so the component goes too — a listed version
// whose package is gone makes restore report the whole feed as invalid.
func (h *Handler) handleDelete(c *gin.Context, repoName, p string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(p, "/v2/packages/"), "/v2/package/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected /v2/package/:id/:version"})
		return
	}
	if _, err := nugetmeta.ParseVersion(parts[1]); err != nil || !nugetmeta.ValidID(parts[0]) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_package_identity"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), searchTimeout)
	defer cancel()
	// Every stored casing of the version: they are one NuGet version (#590).
	paths, err := h.storedNupkgPaths(ctx, repoName, parts[0], parts[1], false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(paths) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "package not found"})
		return
	}
	for _, filePath := range paths {
		if err := base.DeleteArtifact(ctx, h.deps, repoName, filePath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if err := h.deps.Components.DeleteOrphans(ctx, repoName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) serveIndex(c *gin.Context, repoName string) {
	base2 := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(repoName)
	writeNuGetJSON(c, gin.H{
		"version": "3.0.0",
		"resources": []gin.H{
			{"@id": base2 + "/v3/query", "@type": "SearchQueryService"},
			// NuGet.Protocol discovers search using these versioned aliases.
			// Do not advertise 3.5.0 until packageType filtering is implemented.
			{"@id": base2 + "/v3/query", "@type": "SearchQueryService/3.0.0-beta"},
			{"@id": base2 + "/v3/query", "@type": "SearchQueryService/3.0.0-rc"},
			{"@id": base2 + "/v3/registration/", "@type": "RegistrationsBaseUrl"},
			{"@id": base2 + "/v3/registration-semver2/", "@type": "RegistrationsBaseUrl/3.6.0"},
			{"@id": base2 + "/v3/flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
			{"@id": base2 + "/v3/registration/", "@type": "RegistrationsBaseUrl/3.0.0"},
			{"@id": base2 + "/v2/package", "@type": "PackagePublish/2.0.0"},
			{"@id": base2 + "/v2/", "@type": "LegacyGallery/2.0.0"},
		},
	}, false)
}

// packageVersions returns the stored versions of exactly pkgID. Ids are
// stored lowercased on push. Search matches substrings, which would list
// Foo.Abstractions's versions under Foo and stop at one page (#586).
func (h *Handler) packageVersions(ctx context.Context, repoName, pkgID string) ([]domain.Component, error) {
	return base.ExactComponents(ctx, h.deps.Components, domain.SearchParams{
		Repository: repoName, Name: strings.ToLower(pkgID),
	})
}

// storedNupkgPaths returns where the packages of one NuGet version are stored:
// the normalized path every push uses since #590, and the paths of packages
// pushed before it, stored under the version as written in their nuspec.
// With firstOnly, a package at the normalized path ends the search.
func (h *Handler) storedNupkgPaths(ctx context.Context, repoName, id, version string, firstOnly bool) ([]string, error) {
	var out []string
	normalized := nupkgPath(id, version)
	switch _, err := h.deps.Assets.GetByPath(ctx, repoName, normalized); {
	case err == nil:
		out = append(out, normalized)
		if firstOnly {
			return out, nil
		}
	case !errors.Is(err, repository.ErrNotFound):
		return nil, err
	}
	comps, err := h.packageVersions(ctx, repoName, id)
	if err != nil {
		return nil, err
	}
	key := versionKey(version)
	for _, comp := range comps {
		if versionKey(comp.Version) != key {
			continue
		}
		assets, err := h.deps.Assets.ListByComponentID(ctx, comp.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range assets {
			if strings.HasSuffix(a.Path, ".nupkg") && a.Path != normalized {
				out = append(out, a.Path)
			}
		}
	}
	return out, nil
}

// OData v2 compatible FindPackagesById response
type feed struct {
	XMLName xml.Name `xml:"feed"`
	XMLNS   string   `xml:"xmlns,attr"`
	Entries []entry  `xml:"entry"`
}
type entry struct {
	XMLName xml.Name `xml:"entry"`
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Content content  `xml:"content"`
}
type content struct {
	Type string `xml:"type,attr"`
	Src  string `xml:"src,attr"`
}

func (h *Handler) serveFindPackages(c *gin.Context, repoName, pkgID string) {
	repo, err := h.deps.Repos.Get(c.Request.Context(), repoName)
	if err != nil || repo == nil {
		writeQueryError(c, unavailable("search_unavailable"))
		return
	}
	scope, err := h.readScope(c, repo)
	defer finishRead(c)
	if err != nil {
		writeQueryError(c, err)
		return
	}
	records, err := h.exactVersions(c.Request.Context(), scope, pkgID, false)
	if err != nil {
		writeQueryError(c, err)
		return
	}
	prefix := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(scope.Repository.Name)
	f := feed{XMLNS: "http://www.w3.org/2005/Atom"}
	for _, r := range records {
		m := r.Metadata
		f.Entries = append(f.Entries, entry{Title: m.ID + " " + m.Version, ID: prefix + "/v2/Packages(Id='" + m.ID + "',Version='" + m.Version + "')", Content: content{Type: "application/zip", Src: prefix + packagePaths(m.ID, m.Key, false)[1]}})
	}
	c.Header("Content-Type", "application/atom+xml; charset=utf-8")
	c.XML(http.StatusOK, f)
}

func (h *Handler) handlePush(c *gin.Context, repoName string) {
	if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	f, fh, err := c.Request.FormFile("package")
	if err != nil {
		// some clients use "file" as field name
		f, fh, err = c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing package file"})
			return
		}
	}
	defer func() { _ = f.Close() }()

	meta, err := nugetmeta.Read(f, fh.Size)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_nupkg"})
		return
	}
	pkgID, version := meta.ID, meta.Key
	filePath := "/" + pkgID + "/" + version + "/" + pkgID + "." + version + ".nupkg"

	coords := base.Coords{Name: pkgID, Version: version, Extra: map[string]any{"nuget": meta}}
	if _, err := base.StoreArtifact(c.Request.Context(), h.deps,
		repoName, filePath, "application/zip", coords, f, fh.Size); err != nil {
		c.JSON(base.HTTPStatusForError(err), gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusCreated)
}

// fetchAndRewriteNuGetIndex fetches the NuGet v3 service index from upstream,
// rewrites all resource @id URLs to point to this proxy, and returns the result.
// Not cached — fetched live so new resource endpoints appear promptly.
func (h *Handler) fetchAndRewriteNuGetIndex(c *gin.Context, repo *domain.Repository) {
	indexURL, err := nugetServiceIndexURL(repo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Honor explicitly configured service-index paths as well as bare origins.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid upstream URL: " + err.Error()})
		return
	}
	req.Header.Set("Accept", "application/json")

	repoproxy.SetUpstreamAuth(req, repo)
	resp, err := repoproxy.ClientFor(repo).Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream fetch failed: " + err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upstream returned %d", resp.StatusCode)})
		return
	}

	var index map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid upstream index.json: " + err.Error()})
		return
	}

	// Rewrite each resource's @id to point through this proxy.
	// Parse the upstream @id URL, keep only its path, prepend our local base.
	localBase := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + repo.Name

	if resources, ok := index["resources"].([]any); ok {
		for _, r := range resources {
			res, ok := r.(map[string]any)
			if !ok {
				continue
			}
			id, ok := res["@id"].(string)
			if !ok {
				continue
			}
			parsed, err := url.Parse(id)
			if err != nil {
				continue
			}
			res["@id"] = localBase + parsed.RequestURI()
			for _, kind := range searchResourceTypes {
				if res["@type"] == kind {
					res["@id"] = localBase + "/v3/query"
					break
				}
			}
		}
	}

	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.JSON(http.StatusOK, index)
}

// nugetRemoteOrigin normalizes remote_url to the registry's bare origin: a
// legacy configuration carried a /v3 suffix (once the only way the index fetch
// worked), which would double itself onto every already-correct resource path.
func nugetRemoteOrigin(remoteBase string) string {
	u, err := url.Parse(remoteBase)
	if err == nil && u.IsAbs() && u.Host != "" {
		u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
		return strings.TrimRight(u.String(), "/")
	}
	return strings.TrimSuffix(strings.TrimRight(remoteBase, "/"), "/v3")
}

// remoteURLOf reads the repository's remote_url, empty when unset.
func remoteURLOf(repo *domain.Repository) string {
	base, err := repoproxy.RemoteURL(repo)
	if err != nil {
		return ""
	}
	return base
}

func normPath(p string) string {
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// proxyCoords derives component coordinates for a proxied path. A cached
// package must carry its real name and version — the OSV/Trivy scan queries
// by them, so the path-derived fallback name and placeholder version made
// every package pulled through a NuGet proxy invisible to vulnerability
// scanning, the same root cause #336 closed for Cargo.
//
// A proxy repo forwards whatever local path the client requested straight
// onto upstream (upstreamPath := origin + p, above) — unlike a hosted repo,
// it never goes through this file's own "/v3/flatcontainer/" switch-case
// routes. A real client (nuget.exe, dotnet) requests packages at whatever
// address the upstream's own index.json/config.json advertised, which for
// nuget.org and most feeds is "/v3-flatcontainer/" (hyphenated, a sibling of
// "/v3/", not nested inside it — see TestNuGet_ProxyFlatcontainer_
// ResolvesAgainstRealShape). Matching on a specific prefix would silently
// miss that real shape, so this matches by suffix instead: any ".nupkg" path
// is exactly ":id/:ver/:id.:ver.nupkg" — the same 3-segment split
// serveFlatContainerDownload already does for hosted downloads, applied to
// the path's last 3 segments regardless of what comes before them.
// Registration/index pages are versionless metadata and keep the generic
// fallback.
func proxyCoords(p string) base.Coords {
	if !strings.HasSuffix(p, ".nupkg") {
		return base.Coords{}
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 3 {
		return base.Coords{}
	}
	id, ver := parts[len(parts)-3], parts[len(parts)-2]
	if id == "" || ver == "" {
		return base.Coords{}
	}
	return base.Coords{Name: strings.ToLower(id), Version: ver}
}
