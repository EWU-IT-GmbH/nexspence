package nuget

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
)

// Search resources may live on a different origin from the service index.
// Keep paging transparent, and never put query results in the path-keyed
// artifact cache: different searches share the same resource path.
func (h *Handler) serveProxySearch(c *gin.Context, repo *domain.Repository) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Header("Allow", "GET, HEAD")
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	target, err := discoverNuGetResource(c.Request.Context(), repo, searchResourceTypes)
	if err != nil {
		writeQueryError(c, err)
		return
	}
	u, err := validateResourceURL(target)
	if err != nil {
		writeQueryError(c, err)
		return
	}
	query := u.Query()
	for key, values := range c.Request.URL.Query() {
		query[key] = values
	}
	u.RawQuery = query.Encode()
	// Only the resource discovered from this repository's service index is trusted.
	ctx := repoproxy.WithTrustedAuthBases(c.Request.Context(), target)
	body, err := fetchNuGetJSON(ctx, repo, u.String(), remoteCollectionBytes)
	if err != nil {
		writeQueryError(c, err)
		return
	}
	if !json.Valid(body) {
		writeQueryError(c, unavailable("invalid_upstream_response"))
		return
	}
	writeNuGetJSONLimit(c, json.RawMessage(body), false, remoteCollectionBytes)
}
