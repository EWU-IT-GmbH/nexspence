package nuget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
)

const remoteResultLimit = 4000
const remoteCollectionBytes = 32 << 20
const remoteCacheBytes = 64 << 20
const remoteCacheTTL = 30 * time.Second

type remoteCacheEntry struct {
	body    []byte
	expires time.Time
}
type remoteCache struct {
	mu      sync.Mutex
	entries map[string]remoteCacheEntry
	bytes   int
	now     func() time.Time
}

func newRemoteCache() *remoteCache {
	return &remoteCache{entries: map[string]remoteCacheEntry{}, now: time.Now}
}
func (r *remoteCache) get(key string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		return nil, false
	}
	if !r.now().Before(entry.expires) {
		delete(r.entries, key)
		r.bytes -= len(entry.body)
		return nil, false
	}
	return append([]byte(nil), entry.body...), true
}
func (r *remoteCache) put(key string, b []byte) {
	if len(b) > remoteCollectionBytes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[key]; ok {
		r.bytes -= len(old.body)
		delete(r.entries, key)
	}
	for len(r.entries) >= 128 || r.bytes+len(b) > remoteCacheBytes {
		oldest := ""
		var expiry time.Time
		for k, v := range r.entries {
			if oldest == "" || v.expires.Before(expiry) {
				oldest = k
				expiry = v.expires
			}
		}
		if oldest == "" {
			return
		}
		r.bytes -= len(r.entries[oldest].body)
		delete(r.entries, oldest)
	}
	r.entries[key] = remoteCacheEntry{append([]byte(nil), b...), r.now().Add(remoteCacheTTL)}
	r.bytes += len(b)
}
func remoteKey(caller string, repo *domain.Repository, kind string, value any) string {
	b, _ := json.Marshal([]any{caller, repo, kind, value})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func callerCacheKey(user string, roles []string, repo *domain.Repository) string {
	roles = append([]string(nil), roles...)
	sort.Strings(roles)
	b, _ := json.Marshal([]any{user, roles, repo})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type stringList string

func (s *stringList) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = ""
		return nil
	}
	var one string
	if json.Unmarshal(b, &one) == nil {
		*s = stringList(one)
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = stringList(strings.Join(many, ", "))
	return nil
}

type remoteVersion struct {
	Version   string `json:"version"`
	Downloads *int64 `json:"downloads"`
	ID        string `json:"@id"`
}
type remotePackage struct {
	ID          string          `json:"id"`
	Version     string          `json:"version"`
	Versions    []remoteVersion `json:"versions"`
	Description string          `json:"description,omitempty"`
	Authors     stringList      `json:"authors,omitempty"`
	Title       string          `json:"title,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Tags        stringList      `json:"tags,omitempty"`
	LicenseURL  string          `json:"licenseUrl,omitempty"`
	ProjectURL  string          `json:"projectUrl,omitempty"`
	IconURL     string          `json:"iconUrl,omitempty"`
}

func (p remotePackage) metadata(v nugetmeta.Version) nugetmeta.Metadata {
	return nugetmeta.Metadata{Schema: nugetmeta.SchemaVersion, ID: strings.ToLower(p.ID), Version: v.Normalized(), Key: v.Key(), Listed: true, Prerelease: v.Release != "", SemVer2: v.SemVer2(), Description: p.Description, Authors: string(p.Authors), Title: p.Title, Summary: p.Summary, Tags: string(p.Tags), LicenseURL: p.LicenseURL, ProjectURL: p.ProjectURL, IconURL: p.IconURL}
}
func validateRemotePackage(p *remotePackage, o SearchOptions) error {
	if !nugetmeta.ValidID(p.ID) || len(p.Versions) == 0 {
		return unavailable("invalid_upstream_response")
	}
	p.ID = strings.ToLower(p.ID)
	top, err := nugetmeta.ParseVersion(p.Version)
	if err != nil {
		return unavailable("invalid_upstream_response")
	}
	seen := map[string]bool{}
	found := false
	for i := range p.Versions {
		rv := &p.Versions[i]
		v, err := nugetmeta.ParseVersion(rv.Version)
		if err != nil || rv.Downloads == nil || *rv.Downloads < 0 || seen[v.Key()] || (!o.Prerelease && v.Release != "") || (!o.SemVer2 && v.SemVer2()) {
			return unavailable("invalid_upstream_response")
		}
		if _, err := validateResourceURL(rv.ID); err != nil {
			return unavailable("invalid_upstream_response")
		}
		seen[v.Key()] = true
		rv.Version = v.Normalized()
		found = found || v.Key() == top.Key()
	}
	if !found {
		return unavailable("invalid_upstream_response")
	}
	p.Version = top.Normalized()
	return nil
}
func (h *Handler) resource(ctx context.Context, repo *domain.Repository, caller string, kinds []string) (string, error) {
	key := remoteKey(caller, repo, "resource", kinds)
	if b, ok := h.remote.get(key); ok {
		return string(b), nil
	}
	resource, err := discoverNuGetResource(ctx, repo, kinds)
	if err != nil {
		return "", err
	}
	h.remote.put(key, []byte(resource))
	return resource, nil
}
func (h *Handler) remoteSearch(ctx context.Context, repo *domain.Repository, caller string, o SearchOptions) ([]remotePackage, error) {
	key := remoteKey(caller, repo, "search", o)
	if b, ok := h.remote.get(key); ok {
		var out []remotePackage
		err := json.Unmarshal(b, &out)
		return out, err
	}
	resource, err := h.resource(ctx, repo, caller, searchResourceTypes)
	if err != nil {
		return nil, err
	}
	out := []remotePackage{}
	seen := map[string]bool{}
	expected := -1
	size := 0
	for page := 0; page < 64; page++ {
		if len(out) > 3000 {
			return nil, unavailable("remote_search_too_broad")
		}
		u, _ := url.Parse(resource)
		q := u.Query()
		q.Set("q", o.Query)
		q.Set("skip", strconv.Itoa(len(out)))
		q.Set("take", "1000")
		q.Set("prerelease", strconv.FormatBool(o.Prerelease))
		q.Del("packageType")
		if o.SemVer2 {
			q.Set("semVerLevel", "2.0.0")
		} else {
			q.Del("semVerLevel")
		}
		u.RawQuery = q.Encode()
		b, err := fetchNuGetJSON(ctx, repo, u.String(), remoteCollectionBytes-int64(size))
		if err != nil {
			return nil, err
		}
		size += len(b)
		var doc struct {
			TotalHits *int             `json:"totalHits"`
			Data      *[]remotePackage `json:"data"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.TotalHits == nil || doc.Data == nil || *doc.TotalHits < 0 {
			return nil, unavailable("invalid_upstream_response")
		}
		if *doc.TotalHits > remoteResultLimit {
			return nil, unavailable("remote_search_too_broad")
		}
		if expected < 0 {
			expected = *doc.TotalHits
		} else if expected != *doc.TotalHits {
			return nil, unavailable("upstream_results_changed")
		}
		if len(*doc.Data) > 1000 || len(out)+len(*doc.Data) > expected || (len(*doc.Data) == 0 && len(out) < expected) {
			return nil, unavailable("incomplete_upstream_results")
		}
		for _, p := range *doc.Data {
			if err := validateRemotePackage(&p, o); err != nil {
				return nil, err
			}
			if seen[p.ID] {
				return nil, unavailable("upstream_results_changed")
			}
			seen[p.ID] = true
			out = append(out, p)
		}
		if len(out) == expected {
			b, err := json.Marshal(out)
			if err != nil {
				return nil, err
			}
			h.remote.put(key, b)
			return out, nil
		}
	}
	return nil, unavailable("remote_search_too_broad")
}
