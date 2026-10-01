package nuget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
)

const upstreamMetadataTimeout = 30 * time.Second

// Bound outbound NuGet metadata work across handlers and concurrent clients.
var nugetUpstreamSlots = make(chan struct{}, 4)

func validateResourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, unavailable("invalid_upstream_resource")
	}
	return u, nil
}

func nugetServiceIndexURL(repo *domain.Repository) (string, error) {
	remote, err := repoproxy.RemoteURL(repo)
	if err != nil {
		return "", err
	}
	u, err := validateResourceURL(remote)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(u.Path, "/index.json") {
		return u.String(), nil
	}
	if u.RawQuery != "" {
		return "", unavailable("invalid_upstream_resource")
	}
	return nugetRemoteOrigin(remote) + "/v3/index.json", nil
}

// No inbound HTTP request or headers are accepted: authentication comes only
// from proxy_config through the existing SSRF-guarded repository proxy client.
func fetchNuGetJSON(ctx context.Context, repo *domain.Repository, target string, limit int64) ([]byte, error) {
	return fetchNuGetDocument(ctx, repo, target, limit, false)
}

func fetchNuGetDocument(ctx context.Context, repo *domain.Repository, target string, limit int64, optional bool) ([]byte, error) {
	if _, err := validateResourceURL(target); err != nil {
		return nil, err
	}
	// Bound queueing even for callers without a deadline; search and restore
	// keep their own shorter overall deadlines through the parent context.
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	select {
	case nugetUpstreamSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-nugetUpstreamSlots }()
	// Start the network budget only after admission, not while waiting for a slot.
	ctx, cancelFetch := context.WithTimeout(ctx, upstreamMetadataTimeout)
	defer cancelFetch()
	resp, err := repoproxy.FetchUpstreamOnce(ctx, repo, target, "", http.Header{"Accept": []string{"application/json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if optional && resp.StatusCode == http.StatusNotFound {
		return []byte("null"), nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, unavailable("upstream_unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, unavailable("result_too_large")
	}
	return b, nil
}

// Resource type preference is explicit; URLs are always discovered, including
// compatible SearchQueryService aliases and resources on a different host.
func discoverNuGetResource(ctx context.Context, repo *domain.Repository, kinds []string) (string, error) {
	indexURL, err := nugetServiceIndexURL(repo)
	if err != nil {
		return "", err
	}
	b, err := fetchNuGetJSON(ctx, repo, indexURL, maxSearchBytes)
	if err != nil {
		return "", err
	}
	var index struct {
		Resources []struct {
			ID   string `json:"@id"`
			Type string `json:"@type"`
		}
	}
	if err = json.Unmarshal(b, &index); err != nil {
		return "", unavailable("invalid_upstream_response")
	}
	for _, kind := range kinds {
		for _, resource := range index.Resources {
			if resource.Type != kind {
				continue
			}
			if _, err = validateResourceURL(resource.ID); err != nil {
				return "", err
			}
			return resource.ID, nil
		}
	}
	return "", unavailable("upstream_resource_missing")
}

var searchResourceTypes = []string{"SearchQueryService/3.5.0", "SearchQueryService", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.0.0-beta"}
