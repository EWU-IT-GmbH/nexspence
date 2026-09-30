package nuget

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

var _ formats.GroupRequestHandler = (*Handler)(nil)

// ServeGroup handles local v3 resources without invoking each member's HTTP
// handler. Only explicit reads absent from the local catalog may reach proxies.
func (h *Handler) ServeGroup(c *gin.Context, repo *domain.Repository, fallback func([]string)) bool {
	p := normPath(c.Param("path"))
	if p != "/v3/query" && p != "/index.json" && !strings.HasPrefix(p, "/v3/registration/") && !strings.HasPrefix(p, "/v3/registration-semver2/") && !strings.HasPrefix(p, "/v3/flatcontainer/") {
		return false
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Header("Allow", "GET, HEAD")
		writeQueryError(c, &queryError{405, "method_not_allowed"})
		return true
	}
	c.Set("nugetProxyFallback", fallback)
	if p == "/index.json" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), searchTimeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		scope, err := h.hostedScope(c, repo)
		if err != nil {
			writeQueryError(c, err)
			return true
		}
		if !scope.CanRead(p) {
			writeQueryError(c, &queryError{403, "access_denied"})
			return true
		}
		h.serveIndex(c, repo.Name)
	} else {
		h.ServeHTTP(c)
	}
	return true
}

// fallbackGroup never lets an invisible local package be replaced by a remote
// copy. This also prevents a SemVer1 request from exposing a lower-priority copy
// of a local SemVer2 package. Errors remain errors, never empty successful reads.
func (h *Handler) fallbackGroup(c *gin.Context, scope SearchScope, id, key string) bool {
	v, ok := c.Get("nugetProxyFallback")
	if !ok || len(scope.Proxies) == 0 {
		return false
	}
	found := false
	err := h.deps.NuGet.Snapshot(c.Request.Context(), func(s repository.NuGetSnapshot) error {
		return s.Walk(c.Request.Context(), repository.NuGetQuery{Repositories: scope.Members, ExactID: strings.ToLower(id)}, func(candidate repository.NuGetCandidate) error {
			if key == "" || candidate.Key == key {
				found = true
			}
			return nil
		})
	})
	if err != nil {
		writeQueryError(c, err)
		return true
	}
	if found {
		return false
	}
	if !scope.CanRead(normPath(c.Param("path"))) {
		writeQueryError(c, &queryError{403, "access_denied"})
		return true
	}
	if key != "" {
		proxies, err := h.proxyRepositories(c.Request.Context(), scope)
		if err != nil {
			writeQueryError(c, err)
			return true
		}
		for _, proxy := range proxies {
			// Immutable cached bytes remain usable without an upstream metadata request.
			asset, assetErr := h.deps.Assets.GetByPath(c.Request.Context(), proxy.Name, normPath(c.Param("path")))
			selected := assetErr == nil && asset != nil
			if !selected {
				records, err := h.remoteRegistration(c.Request.Context(), proxy, scope.Caller, id)
				if err != nil {
					writeQueryError(c, err)
					return true
				}
				for _, record := range records {
					if record.Metadata.Key == key {
						selected = true
						break
					}
				}
			}
			if selected {
				c.Set("nugetExpectedRemoteVersion", proxy.Name)
				v.(func([]string))([]string{proxy.Name})
				return true
			}
		}
		return false
	}
	v.(func([]string))(scope.Proxies)
	return true
}
