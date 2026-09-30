package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
)

type nugetCatalog struct{ db *pgxpool.Pool }

func NewNuGetCatalog(db *pgxpool.Pool) repository.NuGetCatalog { return &nugetCatalog{db: db} }

type nugetSnapshot struct{ tx pgx.Tx }

func (r *nugetCatalog) Snapshot(ctx context.Context, fn func(repository.NuGetSnapshot) error) error {
	tx, e := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if e = fn(&nugetSnapshot{tx: tx}); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

const nugetReady = `(c.extra->'nuget'->>'schema' = '1'
 AND c.extra->>'nuget_sha256' = a.sha256 AND a.sha256 <> ''
 AND c.extra->>'nuget_path' = a.path
 AND c.extra->'nuget'->>'id' <> '' AND c.extra->'nuget'->>'key' <> ''
 AND c.extra->'nuget'->>'version' <> ''
 AND jsonb_typeof(c.extra->'nuget'->'listed') = 'boolean'
 AND jsonb_typeof(c.extra->'nuget'->'prerelease') = 'boolean'
 AND jsonb_typeof(c.extra->'nuget'->'semVer2') = 'boolean')`

func (s *nugetSnapshot) Ready(ctx context.Context, repos []string) (bool, error) {
	var ready bool
	e := s.tx.QueryRow(ctx, `SELECT NOT EXISTS (
 SELECT 1 FROM components c JOIN repositories r ON r.id=c.repository_id
 LEFT JOIN assets a ON a.component_id=c.id AND lower(a.path) LIKE '%.nupkg'
 WHERE r.name=ANY($1::text[]) AND r.type='hosted' AND c.format='nuget'
 AND (`+nugetReady+`) IS NOT TRUE)`, repos).Scan(&ready)
	return ready, e
}
func (s *nugetSnapshot) Walk(ctx context.Context, q repository.NuGetQuery, fn func(repository.NuGetCandidate) error) error {
	// Escape wildcard syntax without losing the existing name trigram index.
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Query) + "%"
	afterID, afterAsset := "", "00000000-0000-0000-0000-000000000000"
	const batchSize = 256
	for {
		rows, err := s.tx.Query(ctx, `SELECT a.id,c.id,r.name,a.path,
 COALESCE(c.extra->'nuget'->>'id',lower(c.name)),COALESCE(c.extra->'nuget'->>'version',c.version),COALESCE(c.extra->'nuget'->>'key',''),
 COALESCE((c.extra->'nuget'->>'listed')::boolean,true),COALESCE((c.extra->'nuget'->>'prerelease')::boolean,false),
 COALESCE((c.extra->'nuget'->>'semVer2')::boolean,false)
 FROM components c JOIN repositories r ON r.id=c.repository_id JOIN assets a ON a.component_id=c.id
 WHERE r.name=ANY($1::text[]) AND r.type='hosted' AND c.format='nuget'
 AND lower(a.path) LIKE '%.nupkg'
 AND ($2='' OR c.name ILIKE $3 ESCAPE '\')
 AND ($4='' OR lower(c.name)=$4 OR c.extra->'nuget'->>'id'=$4)
 AND ($5='' OR (COALESCE(c.extra->'nuget'->>'id',lower(c.name)) COLLATE "C",a.id)>($5 COLLATE "C",$6::uuid))
 ORDER BY COALESCE(c.extra->'nuget'->>'id',lower(c.name)) COLLATE "C",a.id LIMIT 256`, q.Repositories, q.Query, pattern, q.ExactID, afterID, afterAsset)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var c repository.NuGetCandidate
			if err = rows.Scan(&c.AssetID, &c.ComponentID, &c.Repository, &c.Path, &c.ID, &c.Version, &c.Key, &c.Listed, &c.Prerelease, &c.SemVer2); err != nil {
				break
			}
			if c.Key == "" {
				var v nugetmeta.Version
				v, err = nugetmeta.ParseVersion(c.Version)
				if err != nil {
					break
				}
				c.Key = v.Key()
				c.Version = v.Normalized()
				c.Prerelease = v.Release != ""
				c.SemVer2 = v.SemVer2()
			}
			if err = ctx.Err(); err != nil {
				break
			}
			if err = fn(c); err != nil {
				break
			}
			count++
			afterID, afterAsset = c.ID, c.AssetID
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if count < batchSize {
			return nil
		}
	}
}
func (s *nugetSnapshot) Records(ctx context.Context, ids []string) ([]repository.NuGetRecord, error) {
	rows, e := s.tx.Query(ctx, `SELECT a.id,c.id,r.name,a.path,a.sha256,a.download_count,a.created_at,c.name,c.version,c.extra->'nuget'
 FROM assets a JOIN components c ON c.id=a.component_id JOIN repositories r ON r.id=a.repository_id
 WHERE a.id=ANY($1::uuid[])`, ids)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]repository.NuGetRecord, 0, len(ids))
	metadataBytes := 0
	for rows.Next() {
		var x repository.NuGetRecord
		var b []byte
		var name, version string
		if e = rows.Scan(&x.Asset.ID, &x.Asset.ComponentID, &x.Asset.Repository, &x.Asset.Path, &x.Asset.SHA256, &x.Asset.DownloadCount, &x.Asset.CreatedAt, &name, &version, &b); e != nil {
			return nil, e
		}
		metadataBytes += len(b)
		if metadataBytes > 4<<20 {
			return nil, repository.ErrNuGetResultTooLarge
		}
		if len(b) == 0 || string(b) == "null" {
			v, err := nugetmeta.ParseVersion(version)
			if err != nil {
				return nil, err
			}
			x.Metadata = nugetmeta.Metadata{ID: strings.ToLower(name), Version: v.Normalized(), Key: v.Key(), Listed: true, Prerelease: v.Release != "", SemVer2: v.SemVer2()}
		} else if e = json.Unmarshal(b, &x.Metadata); e != nil {
			return nil, e
		}
		if x.Asset.DownloadCount < 0 {
			return nil, fmt.Errorf("invalid download count")
		}
		out = append(out, x)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("missing nuget assets")
	}
	return out, nil
}
func (r *nugetCatalog) Pending(ctx context.Context, repo, after string, limit int) ([]repository.NuGetBackfillItem, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid batch size")
	}
	rows, e := r.db.Query(ctx, `SELECT a.id,a.component_id,r.name,a.path,a.blob_key,a.blob_store_id,a.size_bytes,a.sha256,c.name,c.version,c.extra
 FROM assets a JOIN components c ON c.id=a.component_id JOIN repositories r ON r.id=a.repository_id
 WHERE r.name=$1 AND r.type='hosted' AND c.format='nuget' AND lower(a.path) LIKE '%.nupkg'
 AND ($2='' OR a.id>NULLIF($2,'')::uuid) AND (`+nugetReady+`) IS NOT TRUE
 ORDER BY a.id LIMIT $3`, repo, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []repository.NuGetBackfillItem{}
	for rows.Next() {
		var x repository.NuGetBackfillItem
		var b []byte
		if e = rows.Scan(&x.Asset.ID, &x.Asset.ComponentID, &x.Asset.Repository, &x.Asset.Path, &x.Asset.BlobKey, &x.Asset.BlobStoreID, &x.Asset.SizeBytes, &x.Asset.SHA256, &x.Name, &x.Version, &b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &x.Extra); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *nugetCatalog) SaveMetadata(ctx context.Context, item repository.NuGetBackfillItem, m nugetmeta.Metadata) (bool, error) {
	extra, e := json.Marshal(map[string]any{"nuget": m, "nuget_sha256": item.Asset.SHA256, "nuget_path": item.Asset.Path})
	if e != nil {
		return false, e
	}
	// Row lock + fingerprint guard prevents a backfill from marking a concurrent replacement ready.
	tx, e := r.db.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var sha string
	e = tx.QueryRow(ctx, `SELECT sha256 FROM assets WHERE id=$1 AND component_id=$2 AND path=$3 FOR UPDATE`, item.Asset.ID, item.Asset.ComponentID, item.Asset.Path).Scan(&sha)
	if e == pgx.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if sha != item.Asset.SHA256 {
		return false, nil
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM assets WHERE component_id=$1 AND lower(path) LIKE '%.nupkg'`, item.Asset.ComponentID).Scan(&n); e != nil {
		return false, e
	}
	if n != 1 {
		return false, fmt.Errorf("ambiguous component assets")
	}
	_, e = tx.Exec(ctx, `UPDATE components SET extra=extra || jsonb_set($2::jsonb,'{nuget,listed}',
 COALESCE(extra->'nuget'->'listed',extra->'listed',$2::jsonb->'nuget'->'listed')),updated_at=NOW() WHERE id=$1`, item.Asset.ComponentID, extra)
	if e != nil {
		return false, e
	}
	return true, tx.Commit(ctx)
}
