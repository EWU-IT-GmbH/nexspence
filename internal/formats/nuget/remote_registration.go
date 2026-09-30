package nuget

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

func (h *Handler) remoteRegistration(ctx context.Context, repo *domain.Repository, caller, id string) ([]repository.NuGetRecord, error) {
	id = strings.ToLower(id)
	if !nugetmeta.ValidID(id) {
		return nil, &queryError{404, "package_not_found"}
	}
	key := remoteKey(caller, repo, "registration", id)
	if b, ok := h.remote.get(key); ok {
		var out []repository.NuGetRecord
		err := json.Unmarshal(b, &out)
		return out, err
	}
	root, err := h.resource(ctx, repo, caller, []string{"RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.0.0", "RegistrationsBaseUrl"})
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(root)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + url.PathEscape(id) + "/index.json"
	u.RawPath = ""
	b, err := fetchNuGetDocument(ctx, repo, u.String(), remoteCollectionBytes, true)
	if err != nil {
		return nil, err
	}
	out := []repository.NuGetRecord{}
	if string(b) == "null" {
		h.remote.put(key, []byte("[]"))
		return out, nil
	}
	type page struct {
		ID    string             `json:"@id"`
		Count *int               `json:"count"`
		Items *[]json.RawMessage `json:"items"`
	}
	var index struct {
		Count *int   `json:"count"`
		Items []page `json:"items"`
	}
	if json.Unmarshal(b, &index) != nil || index.Count == nil || *index.Count != len(index.Items) || len(index.Items) > 128 {
		return nil, unavailable("invalid_upstream_response")
	}
	seen := map[string]bool{}
	size := len(b)
	for _, p := range index.Items {
		if p.Items == nil {
			if _, err := validateResourceURL(p.ID); err != nil {
				return nil, err
			}
			b, err = fetchNuGetJSON(ctx, repo, p.ID, remoteCollectionBytes-int64(size))
			if err != nil {
				return nil, err
			}
			size += len(b)
			if json.Unmarshal(b, &p) != nil {
				return nil, unavailable("invalid_upstream_response")
			}
		}
		if p.Items == nil || p.Count == nil || *p.Count != len(*p.Items) {
			return nil, unavailable("invalid_upstream_response")
		}
		for _, raw := range *p.Items {
			var item struct {
				Catalog json.RawMessage `json:"catalogEntry"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return nil, unavailable("invalid_upstream_response")
			}
			var catalog struct {
				ID                string                      `json:"id"`
				Version           string                      `json:"version"`
				Listed            *bool                       `json:"listed"`
				Published         string                      `json:"published"`
				Description       string                      `json:"description"`
				Authors           stringList                  `json:"authors"`
				Title             string                      `json:"title"`
				Summary           string                      `json:"summary"`
				Tags              stringList                  `json:"tags"`
				LicenseURL        string                      `json:"licenseUrl"`
				LicenseExpression string                      `json:"licenseExpression"`
				ProjectURL        string                      `json:"projectUrl"`
				IconURL           string                      `json:"iconUrl"`
				Dependencies      []nugetmeta.DependencyGroup `json:"dependencyGroups"`
			}
			if json.Unmarshal(item.Catalog, &catalog) != nil || !strings.EqualFold(catalog.ID, id) {
				return nil, unavailable("invalid_upstream_response")
			}
			v, err := nugetmeta.ParseVersion(catalog.Version)
			if err != nil || seen[v.Key()] {
				return nil, unavailable("invalid_upstream_response")
			}
			seen[v.Key()] = true
			published, err := time.Parse(time.RFC3339Nano, catalog.Published)
			if err != nil {
				return nil, unavailable("invalid_upstream_response")
			}
			listed := published.Year() > 1900
			if catalog.Listed != nil {
				listed = *catalog.Listed
			}
			sem2 := v.SemVer2()
			for _, g := range catalog.Dependencies {
				for _, d := range g.Dependencies {
					flag, err := nugetmeta.RangeSemVer2(d.Range)
					if err != nil {
						return nil, unavailable("invalid_upstream_response")
					}
					sem2 = sem2 || flag
				}
			}
			m := nugetmeta.Metadata{Schema: nugetmeta.SchemaVersion, ID: id, Version: v.Normalized(), Key: v.Key(), Listed: listed, Prerelease: v.Release != "", SemVer2: sem2, Description: catalog.Description, Authors: string(catalog.Authors), Title: catalog.Title, Summary: catalog.Summary, Tags: string(catalog.Tags), LicenseURL: catalog.LicenseURL, LicenseExpression: catalog.LicenseExpression, ProjectURL: catalog.ProjectURL, IconURL: catalog.IconURL, DependencyGroups: catalog.Dependencies}
			out = append(out, repository.NuGetRecord{Metadata: m, Asset: domain.Asset{Repository: repo.Name, CreatedAt: published}})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := nugetmeta.ParseVersion(out[i].Metadata.Version)
		b, _ := nugetmeta.ParseVersion(out[j].Metadata.Version)
		return a.Compare(b) < 0
	})
	b, err = json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(b) > remoteCollectionBytes {
		return nil, unavailable("result_too_large")
	}
	h.remote.put(key, b)
	return out, nil
}

func (h *Handler) proxyRepositories(ctx context.Context, scope SearchScope) ([]*domain.Repository, error) {
	if len(scope.Proxies) > 32 {
		return nil, unavailable("too_many_remote_sources")
	}
	out := make([]*domain.Repository, 0, len(scope.Proxies))
	for _, name := range scope.Proxies {
		repo, err := h.deps.Repos.Get(ctx, name)
		if err != nil || repo == nil || !repo.Online || repo.Type != domain.TypeProxy || repo.Format != "nuget" {
			return nil, unavailable("search_unavailable")
		}
		out = append(out, repo)
	}
	return out, nil
}

// Metadata union retains unlisted versions for explicit restore. Filtering happens
// after priority resolution, so a later source cannot resurrect an earlier winner.
func (h *Handler) allVersions(ctx context.Context, scope SearchScope, id string, sem2 bool) ([]repository.NuGetRecord, error) {
	// The policy still checks the actual requested registration hive; only the
	// SemVer classifier is deferred until every source has been merged.
	local, err := h.localVersions(ctx, scope, id, sem2, false)
	if err != nil {
		return nil, err
	}
	winners := map[string]repository.NuGetRecord{}
	mergedBytes := 0
	for _, r := range local {
		winners[r.Metadata.Key] = r
	}
	proxies, err := h.proxyRepositories(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, proxy := range proxies {
		records, err := h.remoteRegistration(ctx, proxy, scope.Caller, id)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			m := r.Metadata
			if _, ok := winners[m.Key]; ok {
				continue
			}
			if !visible(scope, repository.NuGetCandidate{ID: m.ID, Key: m.Key}, sem2) {
				continue
			}
			encoded, err := json.Marshal(r)
			if err != nil {
				return nil, err
			}
			mergedBytes += len(encoded)
			if mergedBytes > remoteCollectionBytes {
				return nil, unavailable("result_too_large")
			}
			winners[m.Key] = r
		}
	}
	out := []repository.NuGetRecord{}
	for _, r := range winners {
		if sem2 || !r.Metadata.SemVer2 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := nugetmeta.ParseVersion(out[i].Metadata.Version)
		b, _ := nugetmeta.ParseVersion(out[j].Metadata.Version)
		return a.Compare(b) < 0
	})
	return out, nil
}
