//go:build integration

package nuget_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nexspence-oss/nexspence/internal/api/handlers"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/group"
	"github.com/nexspence-oss/nexspence/internal/formats/nuget"
	"github.com/nexspence-oss/nexspence/internal/logger"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/repository"
	"github.com/nexspence-oss/nexspence/internal/repository/postgres"
	"github.com/nexspence-oss/nexspence/internal/service"
	"github.com/nexspence-oss/nexspence/internal/storage"
	"github.com/nexspence-oss/nexspence/internal/testutil/pgtest"
	"github.com/stretchr/testify/require"
)

func init() { cleanupIntegration = pgtest.Cleanup }

type hostedFixture struct {
	db      *pgxpool.Pool
	router  *gin.Engine
	handler *nuget.Handler
	repo    *domain.Repository
	deps    formats.Deps
}

func hosted(t *testing.T) hostedFixture {
	t.Helper()
	ctx := context.Background()
	db := pgtest.Pool(t)
	pgtest.Truncate(t, db, "blob_stores", "repositories", "users", "roles", "privileges", "content_selectors")
	blobs := postgres.NewBlobStoreRepo(db)
	bs := &domain.BlobStore{Name: "nuget-test", Type: "local", Config: map[string]any{"path": t.TempDir()}}
	require.NoError(t, blobs.Create(ctx, bs))
	repos := postgres.NewRepositoryRepo(db)
	repo := &domain.Repository{Name: "hosted", Format: "nuget", Type: domain.TypeHosted, Online: true, BlobStoreID: &bs.ID}
	require.NoError(t, repos.Create(ctx, repo))
	rbac := service.NewRBACService(postgres.NewRBACRepo(db), repos, logger.New("error", "json"), true)
	store := storage.NewRegistry(nil)
	d := formats.Deps{Repos: repos, Blobs: blobs, Assets: postgres.NewAssetRepo(db), Components: postgres.NewComponentRepo(db), NuGet: postgres.NewNuGetCatalog(db), RoutingRules: postgres.NewRoutingRuleRepo(db), Registry: store, BaseURL: "https://packages.example.test/root/", RBAC: rbac}
	h := nuget.New(d)
	router := gin.New()
	router.Any("/repository/:repoName/*path", func(c *gin.Context) {
		// Authentication fixture: middleware below remains the production authorization gate.
		switch c.GetHeader("X-Test-Actor") {
		case "anonymous":
		case "reader":
			c.Set("userID", "00000000-0000-0000-0000-000000000001")
		default:
			c.Set("userID", "admin")
			c.Set("roles", []string{"nx-admin"})
		}
		if scope := c.GetHeader("X-Test-Scope"); scope != "" {
			c.Set("tokenScopes", []string{scope})
		}
	}, handlers.RBACMiddleware(rbac, repos), func(c *gin.Context) {
		repo, _ := repos.Get(c.Request.Context(), c.Param("repoName"))
		if repo != nil && repo.Type == domain.TypeGroup {
			group.New(d, map[string]formats.FormatHandler{"nuget": h}).ServeHTTP(c)
		} else {
			h.ServeHTTP(c)
		}
	})
	return hostedFixture{db, router, h, repo, d}
}
func (f hostedFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, httptest.NewRequest("GET", "/repository/hosted"+path, nil))
	return w
}
func (f hostedFixture) push(t *testing.T, id, v string) {
	t.Helper()
	require.Equal(t, 201, pushNupkg(f.router, "hosted", id+"."+v+".nupkg", string(buildNupkg(t, id, v))))
}
func (f hostedFixture) search(t *testing.T, q string) nuget.SearchResponse {
	t.Helper()
	w := f.get(t, "/v3/query"+q)
	require.Equal(t, 200, w.Code, w.Body.String())
	var r nuget.SearchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
	return r
}
func TestHostedSearchEndToEnd(t *testing.T) {
	f := hosted(t)
	r := f.search(t, "")
	require.Zero(t, r.TotalHits)
	require.Empty(t, r.Data)
	f.push(t, "Alpha.Lib", "1.9.0")
	f.push(t, "Alpha.Lib", "1.10.0")
	f.push(t, "Alpha.Lib", "2.0.0-beta.10")
	f.push(t, "Alpha.Lib", "2.0.0-beta.2")
	f.push(t, "Alpha.Lib.Tools", "1.0.0")
	f.push(t, "zeta", "1.0.0")
	r = f.search(t, "?q=ALPHA&take=1")
	require.Equal(t, 2, r.TotalHits)
	require.Len(t, r.Data, 1)
	require.Equal(t, "alpha.lib", r.Data[0].ID)
	require.Equal(t, "1.10.0", r.Data[0].Version)
	require.Len(t, r.Data[0].Versions, 2)
	r = f.search(t, "?q=alpha&skip=1&take=1")
	require.Equal(t, "alpha.lib.tools", r.Data[0].ID)
	r = f.search(t, "?q=alpha.lib&prerelease=true&semVerLevel=2.0.0")
	require.Equal(t, "2.0.0-beta.10", r.Data[0].Version)
	require.Len(t, r.Data[0].Versions, 4)
	for _, v := range r.Data[0].Versions {
		path := strings.TrimPrefix(v.ID, "https://packages.example.test/root/repository/hosted")
		w := f.get(t, path)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		gz, e := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		require.NoError(t, e)
		b, e := io.ReadAll(gz)
		require.NoError(t, e)
		gz.Close()
		var leaf map[string]any
		require.NoError(t, json.Unmarshal(b, &leaf))
		download := strings.TrimPrefix(leaf["packageContent"].(string), "https://packages.example.test/root/repository/hosted")
		w = f.get(t, download)
		require.Equal(t, 200, w.Code)
		require.True(t, bytes.HasPrefix(w.Body.Bytes(), []byte("PK")))
	}
	w := f.get(t, "/v3/registration/alpha.lib/index.json")
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "alpha.lib.tools")
	require.NotContains(t, w.Body.String(), "beta")
	w = f.get(t, "/v3/registration/alpha.lib/7.0.0.json")
	require.Equal(t, 404, w.Code)
	r = f.search(t, "?skip=10000")
	require.Equal(t, 3, r.TotalHits)
	require.Empty(t, r.Data)
	for _, q := range []string{"?q=%25", "?q=%5C", "?q='OR%201=1--"} {
		r = f.search(t, q)
		require.Zero(t, r.TotalHits)
	}
	w = f.get(t, "/index.json")
	require.Contains(t, w.Body.String(), "https://packages.example.test/root/repository/hosted/v3/query")
}
func TestHostedSearchBackfillAndUnlisted(t *testing.T) {
	f := hosted(t)
	f.push(t, "legacy", "1.0.0")
	f.push(t, "legacy", "2.0.0")
	_, e := f.db.Exec(context.Background(), `UPDATE components SET extra='{}' WHERE name='legacy'`)
	require.NoError(t, e)
	w := f.get(t, "/v3/query")
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "metadata_not_ready")
	w = f.get(t, "/v3/flatcontainer/legacy/1.0.0/legacy.1.0.0.nupkg")
	require.Equal(t, 200, w.Code, "explicit legacy downloads remain available")
	report, e := f.handler.Backfill(context.Background(), "hosted", "", 1)
	require.NoError(t, e)
	require.Equal(t, 1, report.Processed)
	require.Empty(t, report.Failures)
	require.NotEmpty(t, report.Next)
	report, e = f.handler.Backfill(context.Background(), "hosted", report.Next, 100)
	require.NoError(t, e)
	require.Equal(t, 1, report.Processed)
	require.Empty(t, report.Failures)
	r := f.search(t, "")
	require.Len(t, r.Data[0].Versions, 2)
	report, e = f.handler.Backfill(context.Background(), "hosted", "", 100)
	require.NoError(t, e)
	require.Zero(t, report.Processed)
	_, e = f.db.Exec(context.Background(), `UPDATE components SET extra=jsonb_set(extra,'{nuget,listed}','false') WHERE version='2.0.0'`)
	require.NoError(t, e)
	r = f.search(t, "")
	require.Equal(t, "1.0.0", r.Data[0].Version)
	require.Len(t, r.Data[0].Versions, 1)
	w = f.get(t, "/v3/registration/legacy/2.0.0.json")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"listed":false`)
	_, e = f.db.Exec(context.Background(), `UPDATE assets SET download_count=42 WHERE path LIKE '%/legacy.1.0.0.nupkg'`)
	require.NoError(t, e)
	r = f.search(t, "")
	require.EqualValues(t, 42, r.Data[0].Versions[0].Downloads)
}
func TestHostedSearchManyVersionsAndGlobalPages(t *testing.T) {
	f := hosted(t)
	// Use genuine pushes so >500 rows exercise metadata creation and the real catalog.
	for i := 0; i < 510; i++ {
		f.push(t, "many", fmt.Sprintf("1.0.%d", i))
	}
	f.push(t, "many.other", "1.0.0")
	r := f.search(t, "?q=many&take=1")
	require.Equal(t, 2, r.TotalHits)
	require.Len(t, r.Data[0].Versions, 510)
	require.Equal(t, "1.0.509", r.Data[0].Version)
	w := f.get(t, "/v3/flatcontainer/many/index.json")
	require.Equal(t, 200, w.Code)
	var v struct{ Versions []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	require.Len(t, v.Versions, 510)
	w = f.get(t, "/v3/registration/many/index.json")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "1.0.509")
	require.NotContains(t, w.Body.String(), "many.other")
}
func TestHostedSearchAuthorization(t *testing.T) {
	f := hosted(t)
	f.push(t, "public", "1.0.0")
	f.push(t, "public", "2.0.0")
	f.push(t, "secret", "1.0.0")
	_, e := f.db.Exec(context.Background(), `INSERT INTO users(id,username,email) VALUES('00000000-0000-0000-0000-000000000001','reader','reader@example.test');
 INSERT INTO roles(id,name) VALUES('00000000-0000-0000-0000-000000000002','reader');
 INSERT INTO user_roles VALUES('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002');`)
	require.NoError(t, e)
	grant := func(expr string) {
		t.Helper()
		_, e = f.db.Exec(context.Background(), `WITH s AS (INSERT INTO content_selectors(name,expression) VALUES($1,$2) RETURNING id), p AS (INSERT INTO privileges(name,type,content_selector_id,attrs) SELECT $1,'repository-content-selector',id,'{"actions":["read"]}' FROM s RETURNING id) INSERT INTO role_privileges SELECT '00000000-0000-0000-0000-000000000002',id FROM p`, uuid.NewString(), expr)
		require.NoError(t, e)
	}
	grant(`repository == "hosted" && path.startsWith("/v3/query")`)
	request := func(actor, scope, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/repository/hosted"+path, nil)
		req.Header.Set("X-Test-Actor", actor)
		req.Header.Set("X-Test-Scope", scope)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		return w
	}
	w := request("reader", "", "/v3/query")
	require.Equal(t, 200, w.Code)
	require.JSONEq(t, `{"totalHits":0,"data":[]}`, w.Body.String())
	grant(`repository == "hosted" && path.startsWith("/v3/flatcontainer/public/index.json")`)
	grant(`repository == "hosted" && path.startsWith("/v3/flatcontainer/public/1.0.0/")`)
	grant(`repository == "hosted" && path.startsWith("/v3/registration/public/")`)
	w = request("reader", "", "/v3/query")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"id":"public"`)
	require.NotContains(t, w.Body.String(), "secret")
	require.NotContains(t, w.Body.String(), "2.0.0")
	w = request("reader", "", "/v3/flatcontainer/public/index.json")
	require.Equal(t, 200, w.Code)
	require.JSONEq(t, `{"versions":["1.0.0"]}`, w.Body.String())
	w = request("reader", "", "/v3/registration/public/2.0.0.json")
	require.Equal(t, 404, w.Code)
	w = request("anonymous", "", "/v3/query")
	require.Equal(t, 401, w.Code)
	w = request("admin", "unknown", "/v3/query")
	require.Equal(t, 403, w.Code)
	f.repo.AllowAnonymous = true
	require.NoError(t, f.deps.Repos.Update(context.Background(), f.repo))
	w = request("anonymous", "", "/v3/query")
	require.Equal(t, 200, w.Code)
}

func TestHostedSearchHTTPParametersAndProxyExclusion(t *testing.T) {
	f := hosted(t)
	for _, tc := range []struct {
		method, query string
		status        int
	}{{"GET", "?take=0", 400}, {"GET", "?take=100", 200}, {"GET", "?take=300", 200}, {"GET", "?take=1000", 200}, {"GET", "?take=1001", 400}, {"GET", "?q=%XX", 400}, {"GET", "?q=%00", 400}, {"GET", "?q=a&q=b", 400}, {"GET", "?semVerLevel=2.0.0&prerelease=true", 200}, {"HEAD", "", 200}, {"HEAD", "?skip=-1", 400}, {"POST", "", 405}, {"DELETE", "", 405}} {
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, httptest.NewRequest(tc.method, "/repository/hosted/v3/query"+tc.query, nil))
		require.Equal(t, tc.status, w.Code, w.Body.String())
		if tc.method == "HEAD" {
			require.Empty(t, w.Body.String())
		}
		if tc.status == 405 {
			require.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
		}
	}
	proxy := &domain.Repository{Name: "proxy", Format: "nuget", Type: domain.TypeProxy, Online: true, BlobStoreID: f.repo.BlobStoreID}
	require.NoError(t, f.deps.Repos.Create(context.Background(), proxy))
	c := &domain.Component{RepositoryID: proxy.ID, Repository: "proxy", Format: "nuget", Name: "secret.remote", Version: "1.0.0"}
	require.NoError(t, f.deps.Components.Create(context.Background(), c))
	r := f.search(t, "")
	require.Zero(t, r.TotalHits)
	// A dependency with SemVer2 bounds hides an otherwise stable package from SemVer1 clients.
	b := buildNupkgWithManifest(t, `<package><metadata><id>dependency.sem2</id><version>1.0.0</version><dependencies><dependency id="Other" version="[1.0.0-alpha.1,2.0)"/></dependencies></metadata></package>`)
	require.Equal(t, 201, pushNupkg(f.router, "hosted", "dep.nupkg", string(b)))
	r = f.search(t, "")
	require.Zero(t, r.TotalHits)
	r = f.search(t, "?semVerLevel=2.0.0")
	require.Equal(t, 1, r.TotalHits)
}
func buildNupkgWithManifest(t *testing.T, m string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, e := z.Create("package.nuspec")
	require.NoError(t, e)
	_, e = w.Write([]byte(m))
	require.NoError(t, e)
	require.NoError(t, z.Close())
	return b.Bytes()
}

func TestHostedBackfillRejectsCorruptionAndCompareAndSwap(t *testing.T) {
	f := hosted(t)
	f.push(t, "legacy", "1.0.0")
	_, e := f.db.Exec(context.Background(), `UPDATE components SET extra='{}'`)
	require.NoError(t, e)
	items, e := f.deps.NuGet.Pending(context.Background(), "hosted", "", 10)
	require.NoError(t, e)
	require.Len(t, items, 1)
	f.push(t, "legacy", "1.0.0") // same bytes; use an actually different manifest for a replacement below
	b := buildNupkgWithManifest(t, `<package><metadata><id>legacy</id><version>1.0.0</version><description>replacement</description></metadata></package>`)
	require.Equal(t, 201, pushNupkg(f.router, "hosted", "legacy.nupkg", string(b)))
	m, e := nugetmeta.Read(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, e)
	saved, e := f.deps.NuGet.SaveMetadata(context.Background(), items[0], m)
	require.NoError(t, e)
	require.False(t, saved, "stale fingerprint must not mark replacement ready")
	_, e = f.db.Exec(context.Background(), `UPDATE components SET extra='{}'; UPDATE assets SET sha256='incorrect'`)
	require.NoError(t, e)
	report, e := f.handler.Backfill(context.Background(), "hosted", "", 10)
	require.NoError(t, e)
	require.False(t, report.Ready)
	require.Len(t, report.Failures, 1)
	require.Equal(t, "blob_digest_mismatch", report.Failures[0].Reason)
	w := f.get(t, "/v3/query")
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "metadata_not_ready")
}

func TestHostedCatalogSnapshotIsConsistent(t *testing.T) {
	f := hosted(t)
	f.push(t, "snapshot", "1.0.0")
	ctx := context.Background()
	require.NoError(t, f.deps.NuGet.Snapshot(ctx, func(s repository.NuGetSnapshot) error {
		ready, e := s.Ready(ctx, []string{"hosted"})
		require.NoError(t, e)
		require.True(t, ready)
		_, e = f.db.Exec(ctx, `UPDATE components SET extra=jsonb_set(extra,'{nuget,listed}','false')`)
		require.NoError(t, e)
		var ids []string
		require.NoError(t, s.Walk(ctx, repository.NuGetQuery{Repositories: []string{"hosted"}}, func(c repository.NuGetCandidate) error {
			require.True(t, c.Listed)
			ids = append(ids, c.AssetID)
			return nil
		}))
		records, e := s.Records(ctx, ids)
		require.NoError(t, e)
		require.Len(t, records, 1)
		require.True(t, records[0].Metadata.Listed)
		return nil
	}))
	r := f.search(t, "")
	require.Zero(t, r.TotalHits, "new requests observe the update")
}

func TestHostedSearchHundredsOfDistinctPackages(t *testing.T) {
	f := hosted(t)
	for i := 0; i < 505; i++ {
		f.push(t, fmt.Sprintf("package%03d", i), "1.0.0")
	}
	r := f.search(t, "?skip=500&take=3")
	require.Equal(t, 505, r.TotalHits)
	require.Len(t, r.Data, 3)
	require.Equal(t, "package500", r.Data[0].ID)
	require.Equal(t, "package502", r.Data[2].ID)
}

func TestHostedRankingBeforePagination(t *testing.T) {
	f := hosted(t)
	for _, id := range []string{"a-foo", "Foo.Extensions", "FOO", "Foo.Core"} {
		f.push(t, id, "1.0.0")
	}
	r := f.search(t, "?q=fOo&take=2")
	require.Equal(t, 4, r.TotalHits)
	require.Equal(t, "foo", r.Data[0].ID)
	require.Equal(t, "foo.core", r.Data[1].ID)
	r = f.search(t, "?q=fOo&skip=2&take=2")
	require.Equal(t, "foo.extensions", r.Data[0].ID)
	require.Equal(t, "a-foo", r.Data[1].ID)
	r = f.search(t, "?take=1")
	require.Equal(t, "a-foo", r.Data[0].ID)
}
