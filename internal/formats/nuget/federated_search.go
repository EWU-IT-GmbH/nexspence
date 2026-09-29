package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

type searchChoice struct {
	candidate repository.NuGetCandidate
	remote    *remotePackage
	source    *domain.Repository
	priority  int
	downloads int64
	matched   bool
}

func (h *Handler) searchRemotes(ctx context.Context, proxies []*domain.Repository, scope SearchScope, o SearchOptions) ([][]remotePackage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([][]remotePackage, len(proxies))
	failures := make([]error, len(proxies))
	jobs := make(chan int, len(proxies))
	for i := range proxies {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var size atomic.Int64
	for worker := 0; worker < 4 && worker < len(proxies); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					failures[i] = ctx.Err()
					continue
				}
				results[i], failures[i] = h.remoteSearch(ctx, proxies[i], scope.Caller, o)
				if failures[i] == nil {
					b, err := json.Marshal(results[i])
					failures[i] = err
					if size.Add(int64(len(b))) > remoteCollectionBytes {
						failures[i] = unavailable("result_too_large")
					}
				}
				if failures[i] != nil {
					cancel()
				}
			}
		}()
	}
	wg.Wait()
	// Prefer the originating failure to cancellation of sibling work.
	for _, err := range failures {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, err
		}
	}
	for _, err := range failures {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (h *Handler) federatedSearch(ctx context.Context, scope SearchScope, o SearchOptions) (SearchResponse, error) {
	out := SearchResponse{Data: []SearchResult{}}
	if scope.CanRead == nil || !scope.CanRead("/v3/query") {
		return out, &queryError{403, "access_denied"}
	}
	if h.deps.NuGet == nil {
		return out, unavailable("search_unavailable")
	}
	proxies, err := h.proxyRepositories(ctx, scope)
	if err != nil {
		return out, err
	}
	remotes, err := h.searchRemotes(ctx, proxies, scope, o)
	if err != nil {
		return out, err
	}
	packages := map[string]map[string]*searchChoice{}
	for priority, docs := range remotes {
		for _, doc := range docs {
			p := doc
			if packages[p.ID] == nil {
				packages[p.ID] = map[string]*searchChoice{}
			}
			for _, rv := range p.Versions {
				v, _ := nugetmeta.ParseVersion(rv.Version)
				candidate := repository.NuGetCandidate{Repository: proxies[priority].Name, ID: p.ID, Version: v.Normalized(), Key: v.Key(), Listed: true, Prerelease: v.Release != "", SemVer2: v.SemVer2()}
				if !visible(scope, candidate, o.SemVer2) {
					continue
				}
				if _, ok := packages[p.ID][v.Key()]; ok {
					continue
				}
				packages[p.ID][v.Key()] = &searchChoice{candidate: candidate, remote: &p, source: proxies[priority], priority: priority, downloads: *rv.Downloads, matched: true}
			}
		}
	}
	err = h.deps.NuGet.Snapshot(ctx, func(snapshot repository.NuGetSnapshot) error {
		ready, err := snapshot.Ready(ctx, scope.Members)
		if err != nil {
			return err
		}
		if !ready {
			return unavailable("metadata_not_ready")
		}
		size := 0
		// A local version shadows a remote copy even if its local ID did not match
		// the upstream's description/tag query. Local candidates stay in one snapshot.
		err = packageCandidates(ctx, snapshot, scope, repository.NuGetQuery{Repositories: scope.Members}, o.SemVer2, func(id string, candidates []repository.NuGetCandidate) error {
			_, remoteMatch := packages[id]
			if !remoteMatch && !strings.Contains(id, strings.ToLower(o.Query)) {
				return nil
			}
			if packages[id] == nil {
				packages[id] = map[string]*searchChoice{}
			}
			for _, candidate := range candidates {
				size += len(candidate.ID) + len(candidate.Version) + 256
				if size > remoteCollectionBytes {
					return unavailable("result_too_large")
				}
				packages[id][candidate.Key] = &searchChoice{candidate: candidate, priority: -1, matched: true}
			}
			return nil
		})
		if err != nil {
			return err
		}
		// A version missing from an earlier source's search may exist there unlisted
		// or may not match that source's query semantics. Do not substitute a later copy.
		for id, versions := range packages {
			for priority, proxy := range proxies {
				needed := false
				for _, choice := range versions {
					if choice.priority > priority {
						needed = true
						break
					}
				}
				if !needed {
					continue
				}
				records, err := h.remoteRegistration(ctx, proxy, scope.Caller, id)
				if err != nil {
					return err
				}
				for _, record := range records {
					key := record.Metadata.Key
					if choice, ok := versions[key]; ok && choice.priority > priority {
						versions[key] = &searchChoice{candidate: repository.NuGetCandidate{ID: id, Key: key}, priority: priority, matched: false}
					}
				}
			}
		}
		ids := []string{}
		for id, versions := range packages {
			for key, choice := range versions {
				c := choice.candidate
				if !choice.matched || !c.Listed || (!o.Prerelease && c.Prerelease) || (!o.SemVer2 && c.SemVer2) {
					delete(versions, key)
				}
			}
			if len(versions) > 0 {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		out.TotalHits = len(ids)
		if o.Skip >= len(ids) {
			return nil
		}
		end := o.Skip + o.Take
		if end > len(ids) {
			end = len(ids)
		}
		ids = ids[o.Skip:end]
		selected := []string{}
		for _, id := range ids {
			for _, choice := range packages[id] {
				if choice.source == nil {
					selected = append(selected, choice.candidate.AssetID)
				}
			}
		}
		records, err := snapshot.Records(ctx, selected)
		if err != nil {
			return err
		}
		byAsset := map[string]repository.NuGetRecord{}
		for _, record := range records {
			byAsset[record.Asset.ID] = record
		}
		root := strings.TrimRight(h.deps.BaseURL, "/") + "/repository/" + url.PathEscape(scope.Repository.Name)
		for _, id := range ids {
			choices := make([]*searchChoice, 0, len(packages[id]))
			for _, choice := range packages[id] {
				choices = append(choices, choice)
			}
			sort.Slice(choices, func(i, j int) bool {
				a, _ := nugetmeta.ParseVersion(choices[i].candidate.Version)
				b, _ := nugetmeta.ParseVersion(choices[j].candidate.Version)
				return a.Compare(b) < 0
			})
			latest := choices[len(choices)-1]
			var metadata nugetmeta.Metadata
			if latest.source == nil {
				record, ok := byAsset[latest.candidate.AssetID]
				if !ok {
					return unavailable("search_unavailable")
				}
				metadata = record.Metadata
			} else {
				version, _ := nugetmeta.ParseVersion(latest.candidate.Version)
				metadata = latest.remote.metadata(version)
				top, _ := nugetmeta.ParseVersion(latest.remote.Version)
				if top.Key() != version.Key() {
					rs, err := h.remoteRegistration(ctx, latest.source, scope.Caller, id)
					if err != nil {
						return err
					}
					found := false
					for _, r := range rs {
						if r.Metadata.Key == version.Key() {
							metadata = r.Metadata
							found = true
							break
						}
					}
					if !found || !metadata.Listed || (!o.SemVer2 && metadata.SemVer2) {
						return unavailable("upstream_results_changed")
					}
				}
			}
			result := SearchResult{ID: id, Version: metadata.Version, Registration: root + registrationRoot(o.SemVer2) + url.PathEscape(id) + "/index.json", Versions: []SearchVersion{}, Description: metadata.Description, Authors: metadata.Authors, Title: metadata.Title, Summary: metadata.Summary, Tags: metadata.Tags, LicenseURL: metadata.LicenseURL, ProjectURL: metadata.ProjectURL, IconURL: metadata.IconURL}
			for _, choice := range choices {
				downloads := choice.downloads
				if choice.source == nil {
					record, ok := byAsset[choice.candidate.AssetID]
					if !ok {
						return unavailable("search_unavailable")
					}
					downloads = record.Asset.DownloadCount
				}
				result.Versions = append(result.Versions, SearchVersion{Version: choice.candidate.Version, Downloads: downloads, ID: root + registrationRoot(o.SemVer2) + url.PathEscape(id) + "/" + url.PathEscape(choice.candidate.Key) + ".json"})
			}
			out.Data = append(out.Data, result)
		}
		return nil
	})
	return out, err
}
