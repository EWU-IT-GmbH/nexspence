package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

// NuGetCatalog is an in-memory catalog for format unit tests. SQL/snapshot behavior
// is verified separately against PostgreSQL by the hosted integration suite.
type NuGetCatalog struct {
	Components *ComponentRepo
	Assets     *AssetRepo
}
type nugetMemorySnapshot struct{ records []repository.NuGetRecord }

func (r *NuGetCatalog) Snapshot(ctx context.Context, fn func(repository.NuGetSnapshot) error) error {
	r.Components.mu.Lock()
	defer r.Components.mu.Unlock()
	r.Assets.mu.Lock()
	defer r.Assets.mu.Unlock()
	if r.Components.Err != nil {
		return r.Components.Err
	}
	if r.Assets.Err != nil {
		return r.Assets.Err
	}
	s := &nugetMemorySnapshot{}
	for _, a := range r.Assets.byID {
		if !strings.HasSuffix(a.Path, ".nupkg") {
			continue
		}
		c := r.Components.components[a.ComponentID]
		if c == nil {
			continue
		}
		var m nugetmeta.Metadata
		b, _ := json.Marshal(c.Extra["nuget"])
		if e := json.Unmarshal(b, &m); e != nil {
			return e
		}
		if m.ID == "" {
			v, e := nugetmeta.ParseVersion(c.Version)
			if e != nil {
				return e
			}
			m = nugetmeta.Metadata{ID: strings.ToLower(c.Name), Version: v.Normalized(), Key: v.Key(), Listed: true, Prerelease: v.Release != "", SemVer2: v.SemVer2()}
		}
		cand := repository.NuGetCandidate{AssetID: a.ID, ComponentID: c.ID, Repository: c.Repository, Path: a.Path, ID: m.ID, Version: m.Version, Key: m.Key, Listed: m.Listed, Prerelease: m.Prerelease, SemVer2: m.SemVer2}
		s.records = append(s.records, repository.NuGetRecord{Candidate: cand, Metadata: m, Asset: *a})
	}
	return fn(s)
}
func (s *nugetMemorySnapshot) Ready(_ context.Context, repos []string) (bool, error) {
	for _, r := range s.records {
		if containsRepo(repos, r.Asset.Repository) && r.Metadata.Schema != nugetmeta.SchemaVersion {
			return false, nil
		}
	}
	return true, nil
}
func containsRepo(repos []string, name string) bool {
	for _, r := range repos {
		if r == name {
			return true
		}
	}
	return false
}
func (s *nugetMemorySnapshot) Walk(ctx context.Context, q repository.NuGetQuery, fn func(repository.NuGetCandidate) error) error {
	records := append([]repository.NuGetRecord{}, s.records...)
	sort.Slice(records, func(i, j int) bool {
		if records[i].Candidate.ID != records[j].Candidate.ID {
			return records[i].Candidate.ID < records[j].Candidate.ID
		}
		return records[i].Asset.ID < records[j].Asset.ID
	})
	for _, r := range records {
		c := r.Candidate
		if !containsRepo(q.Repositories, c.Repository) || q.ExactID != "" && c.ID != q.ExactID || !strings.Contains(strings.ToLower(c.ID), strings.ToLower(q.Query)) {
			continue
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := fn(c); e != nil {
			return e
		}
	}
	return nil
}
func (s *nugetMemorySnapshot) Records(_ context.Context, ids []string) ([]repository.NuGetRecord, error) {
	out := []repository.NuGetRecord{}
	for _, id := range ids {
		found := false
		for _, r := range s.records {
			if id == r.Asset.ID {
				out = append(out, r)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("missing asset")
		}
	}
	return out, nil
}
func (r *NuGetCatalog) Pending(context.Context, string, string, int) ([]repository.NuGetBackfillItem, error) {
	return nil, fmt.Errorf("backfill requires PostgreSQL integration fixture")
}
func (r *NuGetCatalog) SaveMetadata(context.Context, repository.NuGetBackfillItem, nugetmeta.Metadata) (bool, error) {
	return false, fmt.Errorf("backfill requires PostgreSQL integration fixture")
}
