package nuget

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats/base"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

// Restore may queue behind other packages and fetch multiple registration pages.
// Keep its overall budget separate from the interactive search deadline.
const restoreTimeout = 2 * time.Minute

func gzipJSON(b []byte) ([]byte, error) {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, e := w.Write(b); e != nil {
		return nil, e
	}
	if e := w.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}

func (h *Handler) exactVersions(ctx context.Context, scope SearchScope, id string, sem2 bool) ([]repository.NuGetRecord, error) {
	return h.localVersions(ctx, scope, id, sem2, true)
}
func (h *Handler) localVersions(ctx context.Context, scope SearchScope, id string, sem2, filterSem2 bool) ([]repository.NuGetRecord, error) {
	out := []repository.NuGetRecord{}
	if h.deps.NuGet == nil {
		return nil, unavailable("search_unavailable")
	}
	if !nugetmeta.ValidID(id) {
		return out, &queryError{404, "package_not_found"}
	}
	id = strings.ToLower(id)
	e := h.deps.NuGet.Snapshot(ctx, func(s repository.NuGetSnapshot) error {
		ids := []string{}
		// Exact legacy reads remain available before backfill. Search separately requires Ready.
		e := packageCandidates(ctx, s, scope, repository.NuGetQuery{Repositories: scope.Members, ExactID: id}, sem2, func(_ string, cs []repository.NuGetCandidate) error {
			for _, c := range cs {
				if !filterSem2 || sem2 || !c.SemVer2 {
					ids = append(ids, c.AssetID)
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		out, e = s.Records(ctx, ids)
		return e
	})
	if e != nil {
		return nil, e
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := nugetmeta.ParseVersion(out[i].Metadata.Version)
		b, _ := nugetmeta.ParseVersion(out[j].Metadata.Version)
		return a.Compare(b) < 0
	})
	return out, nil
}
func (h *Handler) readScope(c *gin.Context, repo *domain.Repository) (SearchScope, error) {
	// All exact-read operations use a deadline as well, including the privilege snapshot.
	ctx, cancel := context.WithTimeout(c.Request.Context(), restoreTimeout)
	// The handler calls the returned cleanup through the request lifecycle below.
	c.Set("nugetReadCancel", cancel)
	c.Request = c.Request.WithContext(ctx)
	scope, err := h.hostedScope(c, repo)
	if err == nil && !scope.CanRead(normPath(c.Param("path"))) {
		err = &queryError{403, "access_denied"}
	}
	return scope, err
}
func finishRead(c *gin.Context) {
	if v, ok := c.Get("nugetReadCancel"); ok {
		v.(context.CancelFunc)()
	}
}
func (h *Handler) serveHostedVersions(c *gin.Context, repo *domain.Repository, p string) {
	scope, e := h.readScope(c, repo)
	defer finishRead(c)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, "/v3/flatcontainer/"), "/index.json")
	policy := scope.CanRead
	scope.CanRead = func(path string) bool {
		if strings.HasPrefix(path, registrationRoot(true)) {
			return policy(path) || policy(strings.Replace(path, registrationRoot(true), registrationRoot(false), 1))
		}
		return policy(path)
	}
	rs, e := h.allVersions(c.Request.Context(), scope, id, true)
	if e != nil {
		writeQueryError(c, e)
		return
	}

	versions := []string{}
	for _, r := range rs {
		versions = append(versions, r.Metadata.Key)
	}
	writeNuGetJSON(c, gin.H{"versions": versions}, false)
}
func (h *Handler) serveHostedRegistration(c *gin.Context, repo *domain.Repository, p string) {
	sem2 := strings.HasPrefix(p, registrationRoot(true))
	root := registrationRoot(sem2)
	rest := strings.TrimPrefix(p, root)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	id := strings.ToLower(parts[0])

	if parts[1] != "index.json" {
		v, err := nugetmeta.ParseVersion(strings.TrimSuffix(parts[1], ".json"))
		if err != nil {
			writeQueryError(c, &queryError{404, "package_not_found"})
			return
		}
		_ = v
	}
	scope, e := h.readScope(c, repo)
	defer finishRead(c)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	rs, e := h.allVersions(c.Request.Context(), scope, id, sem2)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	if len(rs) == 0 {

		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	prefix := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(scope.Repository.Name)
	index := prefix + root + url.PathEscape(id) + "/index.json"
	entries := []gin.H{}
	for _, r := range rs {
		m := r.Metadata
		leaf := prefix + root + url.PathEscape(id) + "/" + url.PathEscape(m.Key) + ".json"
		content := prefix + packagePaths(id, m.Key, sem2)[1]
		catalog := gin.H{"@id": index + "#catalog/" + url.PathEscape(m.Key), "id": id, "version": m.Version, "listed": m.Listed, "published": r.Asset.CreatedAt.UTC().Format(time.RFC3339)}
		if m.Description != "" {
			catalog["description"] = m.Description
		}
		if m.Authors != "" {
			catalog["authors"] = m.Authors
		}
		if len(m.DependencyGroups) > 0 {
			catalog["dependencyGroups"] = m.DependencyGroups
		}
		if m.LicenseExpression != "" {
			catalog["licenseExpression"] = m.LicenseExpression
		}
		entry := gin.H{"@id": leaf, "catalogEntry": catalog, "packageContent": content}
		if parts[1] != "index.json" {
			requested, e := nugetmeta.ParseVersion(strings.TrimSuffix(parts[1], ".json"))
			if e != nil {
				writeQueryError(c, unavailable("search_unavailable"))
				return
			}
			if requested.Key() == m.Key {
				delete(entry, "catalogEntry")
				entry["listed"] = m.Listed
				entry["published"] = r.Asset.CreatedAt.UTC().Format(time.RFC3339)
				entry["registration"] = index
				writeNuGetJSON(c, entry, sem2)
				return
			}
		}
		entries = append(entries, entry)
	}
	if parts[1] != "index.json" {

		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	page := gin.H{"@id": index + "#page", "parent": index, "count": len(entries), "lower": rs[0].Metadata.Key, "upper": rs[len(rs)-1].Metadata.Key, "items": entries}
	writeNuGetJSON(c, gin.H{"@id": index, "count": 1, "items": []gin.H{page}}, sem2)
}
func (h *Handler) serveHostedDownload(c *gin.Context, repo *domain.Repository, p string) {
	rest := strings.TrimPrefix(p, "/v3/flatcontainer/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !nugetmeta.ValidID(parts[0]) {
		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	v, e := nugetmeta.ParseVersion(parts[1])
	if e != nil {
		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	id := strings.ToLower(parts[0])
	if !strings.EqualFold(parts[2], parts[0]+"."+parts[1]+".nupkg") {
		writeQueryError(c, &queryError{404, "package_not_found"})
		return
	}
	scope, e := h.readScope(c, repo)
	defer finishRead(c)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	// Download does not require access to metadata endpoints. A client can restore
	// an explicitly named package with only content access; discovery requires all paths.
	policy := scope.CanRead
	scope.CanRead = func(path string) bool {
		if strings.HasSuffix(path, ".nupkg") {
			return policy(path)
		}
		return true
	}
	rs, e := h.exactVersions(c.Request.Context(), scope, id, true)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	for _, r := range rs {
		if r.Metadata.Key == v.Key() {
			if c.Request.Method == http.MethodHead {
				c.Header("Content-Type", "application/zip")
				a, e := h.deps.Assets.Get(c.Request.Context(), r.Asset.ID)
				if e != nil {
					writeQueryError(c, e)
					return
				}
				c.Header("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
				c.Status(200)
				return
			}
			rc, a, e := base.FetchArtifact(c.Request.Context(), h.deps, r.Asset.Repository, r.Asset.Path)
			if e != nil {
				writeQueryError(c, unavailable("search_unavailable"))
				return
			}
			defer rc.Close()
			c.DataFromReader(200, a.SizeBytes, "application/zip", rc, nil)
			return
		}
	}
	if h.fallbackGroup(c, scope, id, v.Key()) {
		return
	}
	writeQueryError(c, &queryError{404, "package_not_found"})
}
