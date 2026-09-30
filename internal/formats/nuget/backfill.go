package nuget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
	"github.com/nexspence-oss/nexspence/internal/storage"
)

const maxBackfillPackageBytes = 512 << 20

// BackfillReport's cursor can be persisted by the operator. Starting again is safe:
// successfully validated fingerprints are excluded by the catalog query.
type BackfillReport struct {
	Ready     bool              `json:"ready"`
	Processed int               `json:"processed"`
	Next      string            `json:"next,omitempty"`
	Failures  []BackfillFailure `json:"failures"`
}
type BackfillFailure struct {
	AssetID string `json:"assetId"`
	Reason  string `json:"reason"`
}

// Backfill is an explicitly invoked maintenance operation. It never runs from Search.
func (h *Handler) Backfill(ctx context.Context, repoName, after string, limit int) (BackfillReport, error) {
	report := BackfillReport{Failures: []BackfillFailure{}}
	if h.deps.NuGet == nil {
		return report, unavailable("search_unavailable")
	}
	repo, e := h.deps.Repos.Get(ctx, repoName)
	if e != nil {
		return report, e
	}
	if repo == nil || repo.Type != domain.TypeHosted || repo.Format != "nuget" {
		return report, invalid("hosted_nuget_required")
	}
	if after != "" {
		if _, e = uuid.Parse(after); e != nil {
			return report, invalid("invalid_cursor")
		}
	}
	if limit < 1 || limit > 100 {
		return report, invalid("invalid_limit")
	}
	items, e := h.deps.NuGet.Pending(ctx, repoName, after, limit)
	if e != nil {
		return report, e
	}
	for _, item := range items {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		report.Next = item.Asset.ID
		if e = h.backfillItem(ctx, item); e != nil {
			report.Failures = append(report.Failures, BackfillFailure{item.Asset.ID, e.Error()})
			continue
		}
		report.Processed++
	}
	if len(items) < limit {
		report.Next = ""
	}
	err := h.deps.NuGet.Snapshot(ctx, func(s repository.NuGetSnapshot) error {
		var e error
		report.Ready, e = s.Ready(ctx, []string{repoName})
		return e
	})
	return report, err
}
func (h *Handler) backfillItem(ctx context.Context, item repository.NuGetBackfillItem) error {
	a := item.Asset
	if a.SizeBytes < 1 || a.SizeBytes > maxBackfillPackageBytes {
		return fmt.Errorf("package_size_out_of_range")
	}
	store := h.deps.BlobStore
	if h.deps.Registry != nil {
		meta, e := h.deps.Blobs.GetByID(ctx, a.BlobStoreID)
		if e != nil || meta == nil {
			return fmt.Errorf("blob_store_unavailable")
		}
		store, e = h.deps.Registry.Get(ctx, storage.BlobStoreDescriptor{ID: meta.ID, Type: meta.Type, Config: meta.Config})
		if e != nil {
			return fmt.Errorf("blob_store_unavailable")
		}
	}
	if store == nil {
		return fmt.Errorf("blob_store_unavailable")
	}
	rc, _, e := store.Get(ctx, a.BlobKey)
	if e != nil {
		return fmt.Errorf("blob_unavailable")
	}
	defer rc.Close()
	f, e := os.CreateTemp("", "nexspence-nuget-backfill-*.nupkg")
	if e != nil {
		return fmt.Errorf("staging_unavailable")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	digest := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, digest), io.LimitReader(rc, maxBackfillPackageBytes+1))
	if e != nil || n != a.SizeBytes {
		return fmt.Errorf("blob_size_mismatch")
	}
	if hex.EncodeToString(digest.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("blob_digest_mismatch")
	}
	m, e := nugetmeta.Read(f, n)
	if e != nil {
		return fmt.Errorf("invalid_nuspec")
	}
	old, e := nugetmeta.ParseVersion(item.Version)
	if e != nil || m.ID != strings.ToLower(item.Name) || m.Key != old.Key() {
		return fmt.Errorf("coordinate_mismatch")
	}
	// Listed is server state, not nuspec data. Preserve an explicit legacy marker.
	if listed, ok := item.Extra["listed"].(bool); ok {
		m.Listed = listed
	}
	if raw, ok := item.Extra["nuget"]; ok {
		b, _ := json.Marshal(raw)
		var prior struct {
			Listed *bool `json:"listed"`
		}
		if json.Unmarshal(b, &prior) == nil && prior.Listed != nil {
			m.Listed = *prior.Listed
		}
	}
	saved, e := h.deps.NuGet.SaveMetadata(ctx, item, m)
	if e != nil {
		return fmt.Errorf("metadata_save_failed")
	}
	if !saved {
		return fmt.Errorf("asset_changed_retry")
	}
	return nil
}

// ServeBackfill must be mounted behind the existing authenticated nx-admin gate.
func (h *Handler) ServeBackfill(c *gin.Context) {
	if raw, ok := c.Get("tokenScopes"); ok {
		ss, _ := raw.([]string)
		allowed := len(ss) == 0
		for _, s := range ss {
			if s == "write" || s == "delete" {
				allowed = true
			}
		}
		if !allowed {
			writeQueryError(c, &queryError{403, "access_denied"})
			return
		}
	}

	limit := 20
	if v, ok := c.GetQuery("limit"); ok {
		var e error
		limit, e = strconv.Atoi(v)
		if e != nil {
			writeQueryError(c, invalid("invalid_limit"))
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	report, e := h.Backfill(ctx, c.Param("repoName"), c.Query("after"), limit)
	if e != nil {
		writeQueryError(c, e)
		return
	}
	c.JSON(http.StatusOK, report)
}
