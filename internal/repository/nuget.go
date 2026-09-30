package repository

import (
	"context"
	"errors"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
)

var ErrNuGetResultTooLarge = errors.New("nuget metadata exceeds response budget")

// NuGetCandidate is the small identity/policy projection used BEFORE package paging.
// Descriptive metadata is loaded only for the selected packages.
type NuGetCandidate struct {
	AssetID, ComponentID, Repository, Path, ID, Version, Key string
	Listed, Prerelease, SemVer2                              bool
}
type NuGetQuery struct {
	Repositories   []string
	Query, ExactID string
}
type NuGetRecord struct {
	Candidate NuGetCandidate
	Metadata  nugetmeta.Metadata
	Asset     domain.Asset
}
type NuGetSnapshot interface {
	Ready(context.Context, []string) (bool, error)
	Walk(context.Context, NuGetQuery, func(NuGetCandidate) error) error
	Records(context.Context, []string) ([]NuGetRecord, error)
}
type NuGetBackfillItem struct {
	Asset         domain.Asset
	Name, Version string
	Extra         map[string]any
}

// NuGetCatalog keeps count, package selection and version loading in one read snapshot.
type NuGetCatalog interface {
	Snapshot(context.Context, func(NuGetSnapshot) error) error
	Pending(context.Context, string, string, int) ([]NuGetBackfillItem, error)
	SaveMetadata(context.Context, NuGetBackfillItem, nugetmeta.Metadata) (bool, error)
}
