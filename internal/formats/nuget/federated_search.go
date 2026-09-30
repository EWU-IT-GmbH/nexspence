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
	metadata  *nugetmeta.Metadata
	source    *domain.Repository
	priority  int
	downloads int64
	matched   bool
}

func (h *Handler) searchRemotes(ctx context.Context, proxies []*domain.Repository, scope SearchScope, o SearchOptions, offsets []int, done []bool) ([]remoteSearchPage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]remoteSearchPage, len(proxies))
	failures := make([]error, len(proxies))
	jobs := make(chan int, len(proxies))
	for i := range proxies {
		if !done[i] {
			jobs <- i
		}
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
				pageOptions := o
				pageOptions.Skip = offsets[i]
				results[i], failures[i] = h.remoteSearch(ctx, proxies[i], scope.Caller, pageOptions)
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
	// Fetch an exact ID independently of its position in a broad upstream search.
	exact := make([][]remotePackage, len(proxies))
	query := strings.ToLower(strings.TrimSpace(o.Query))
	if query != "" && nugetmeta.ValidID(query) {
		exactOptions := o
		exactOptions.Query, exactOptions.Skip, exactOptions.Take = "packageid:"+query, 0, 1
		pages, err := h.searchRemotes(ctx, proxies, scope, exactOptions, make([]int, len(proxies)), make([]bool, len(proxies)))
		if err != nil {
			return out, err
		}
		for i, page := range pages {
			for _, p := range page.Data {
				if strings.EqualFold(p.ID, query) {
					exact[i] = append(exact[i], p)
				}
			}
		}
	}
	remotes := make([][]remotePackage, len(proxies))
	offsets := make([]int, len(proxies))
	totals := make([]int, len(proxies))
	done := make([]bool, len(proxies))
	seen := make([]map[string]bool, len(proxies))
	for i := range proxies {
		totals[i] = -1
		seen[i] = map[string]bool{}
	}
	pageOptions := o
	pageOptions.Take = min(1000, max(100, o.Skip+o.Take+1))
	bytes := 0
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		pages, err := h.searchRemotes(ctx, proxies, scope, pageOptions, offsets, done)
		if err != nil {
			return out, err
		}
		complete := true
		frontier := int(^uint(0) >> 1)
		for i, page := range pages {
			if !done[i] {
				if totals[i] >= 0 && totals[i] != page.TotalHits {
					return out, unavailable("upstream_results_changed")
				}
				totals[i] = page.TotalHits
				for _, p := range page.Data {
					if seen[i][p.ID] {
						return out, unavailable("upstream_results_changed")
					}
					seen[i][p.ID] = true
				}
				b, _ := json.Marshal(page.Data)
				bytes += len(b)
				if bytes > remoteCollectionBytes {
					return out, unavailable("result_too_large")
				}
				remotes[i] = append(remotes[i], page.Data...)
				offsets[i] += len(page.Data)
				done[i] = offsets[i] >= page.TotalHits
			}
			if !done[i] {
				complete = false
				frontier = min(frontier, offsets[i])
			}
		}
		out, err = h.mergeSearchWindow(ctx, scope, o, proxies, remotes, exact, frontier)
		if err != nil {
			return out, err
		}
		if complete {
			return out, nil
		}
		// One visible look-ahead result is proof of another page. Do not leak
		// upstream totals, which include duplicates and potentially hidden IDs.
		if out.TotalHits > o.Skip+o.Take {
			out.TotalHits = o.Skip + len(out.Data) + 1
			return out, nil
		}
	}
}

// Hosted exact, proxy exact, hosted prefix, other hosted, then proxy relevance.
// The unfinished-source frontier applies only to ordinary proxy hits. Hosted
// hits and the independently fetched exact hits are known before paging.
func (h *Handler) mergeSearchWindow(ctx context.Context, scope SearchScope, o SearchOptions, proxies []*domain.Repository, remotes, exact [][]remotePackage, frontier int) (SearchResponse, error) {
	out := SearchResponse{Data: []SearchResult{}}
	type rank struct{ tier, position, source int }
	ranks := map[string]rank{}
	remember := func(id string, position, source int) {
		prior, ok := ranks[id]
		if !ok || position < prior.position || (position == prior.position && source < prior.source) {
			ranks[id] = rank{4, position, source}
		}
	}
	for source, docs := range remotes {
		for position, p := range docs {
			remember(p.ID, position, source+1)
		}
	}
	combined := make([][]remotePackage, len(remotes))
	for source := range remotes {
		combined[source] = append(combined[source], exact[source]...)
		for _, p := range exact[source] {
			ranks[p.ID] = rank{1, 0, source + 1}
		}
		// Exact packages are inserted once; their regular-page position still counts
		// towards upstream offsets, so removing the duplicate cannot skip a raw hit.
		for _, p := range remotes[source] {
			isExact := false
			for _, e := range exact[source] {
				if p.ID == e.ID {
					isExact = true
					break
				}
			}
			if !isExact {
				combined[source] = append(combined[source], p)
			}
		}
	}
	packages := map[string]map[string]*searchChoice{}
	for priority, docs := range combined {
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
	err := h.deps.NuGet.Snapshot(ctx, func(snapshot repository.NuGetSnapshot) error {
		ready, err := snapshot.Ready(ctx, scope.Members)
		if err != nil {
			return err
		}
		if !ready {
			return unavailable("metadata_not_ready")
		}
		size := 0
		localIDs := []string{}
		// A local version shadows a remote copy even if its local ID did not match
		// the upstream's description/tag query. Local candidates stay in one snapshot.
		err = packageCandidates(ctx, snapshot, scope, repository.NuGetQuery{Repositories: scope.Members}, o.SemVer2, func(id string, candidates []repository.NuGetCandidate) error {
			if strings.Contains(id, strings.ToLower(o.Query)) {
				for _, c := range candidates {
					if c.Listed && (o.Prerelease || !c.Prerelease) && (o.SemVer2 || !c.SemVer2) {
						localIDs = append(localIDs, id)
						break
					}
				}
			}
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
		score := func(id string) int { return hostedSearchTier(id, o.Query) }
		sort.Slice(localIDs, func(i, j int) bool {
			a, b := score(localIDs[i]), score(localIDs[j])
			if a != b {
				return a < b
			}
			return localIDs[i] < localIDs[j]
		})
		for i, id := range localIDs {
			ranks[id] = rank{score(id), i, 0}
		}
		// A version missing from an earlier source's search may exist there unlisted
		// or may not match that source's query semantics. Do not substitute a later copy.
		for id, versions := range packages {
			if ranks[id].tier == 4 && ranks[id].position >= frontier {
				continue
			}
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
						m := record.Metadata
						candidate := repository.NuGetCandidate{Repository: proxy.Name, ID: id, Key: key, Version: m.Version, Listed: m.Listed, Prerelease: m.Prerelease, SemVer2: m.SemVer2}
						versions[key] = &searchChoice{candidate: candidate, metadata: &m, source: proxy, priority: priority, matched: visible(scope, candidate, o.SemVer2)}
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
			if len(versions) > 0 && (ranks[id].tier < 4 || ranks[id].position < frontier) {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool {
			a, b := ranks[ids[i]], ranks[ids[j]]
			if a.tier != b.tier {
				return a.tier < b.tier
			}
			if a.position != b.position {
				return a.position < b.position
			}
			if a.source != b.source {
				return a.source < b.source
			}
			return ids[i] < ids[j]
		})
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
			} else if latest.metadata != nil {
				metadata = *latest.metadata
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
