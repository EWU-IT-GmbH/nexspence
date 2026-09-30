//go:build integration

package nuget_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats/nuget"
	"github.com/stretchr/testify/require"
)

func (f hostedFixture) addRepo(t *testing.T, name string, typ domain.RepoType, members ...string) *domain.Repository {
	t.Helper()
	r := &domain.Repository{Name: name, Type: typ, Format: "nuget", Online: true, BlobStoreID: f.repo.BlobStoreID, FormatConfig: map[string]any{"member_names": members}}
	require.NoError(t, f.deps.Repos.Create(context.Background(), r))
	return r
}
func (f hostedFixture) requestRepo(t *testing.T, method, repo, path, actor string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/repository/"+repo+path, nil)
	r.Header.Set("X-Test-Actor", actor)
	f.router.ServeHTTP(w, r)
	return w
}
func (f hostedFixture) groupSearch(t *testing.T, repo, query, actor string) nuget.SearchResponse {
	t.Helper()
	w := f.requestRepo(t, "GET", repo, "/v3/query"+query, actor)
	require.Equal(t, 200, w.Code, w.Body.String())
	var out nuget.SearchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}
func (f hostedFixture) pushTo(t *testing.T, repo, id, version, description string) []byte {
	t.Helper()
	b := buildNupkgWithManifest(t, fmt.Sprintf(`<package><metadata><id>%s</id><version>%s</version><description>%s</description></metadata></package>`, id, version, description))
	require.Equal(t, 201, pushNupkg(f.router, repo, "package.nupkg", string(b)))
	return b
}
func TestGroupSearchPriorityRegistrationAndDownloads(t *testing.T) {
	f := hosted(t)
	f.addRepo(t, "empty", domain.TypeHosted)
	f.addRepo(t, "second", domain.TypeHosted)
	f.addRepo(t, "all", domain.TypeGroup, "empty", "hosted", "second")
	winner := f.pushTo(t, "hosted", "Shared.Lib", "1.0.0+winner", "first member")
	f.pushTo(t, "second", "SHARED.LIB", "1.0.0.0+other", "second member")
	f.pushTo(t, "second", "Shared.Lib", "2.0.0", "new version")
	f.pushTo(t, "second", "zeta", "1.0.0", "second only")
	r := f.groupSearch(t, "all", "?semVerLevel=2.0.0", "admin")
	require.Equal(t, 2, r.TotalHits)
	require.Len(t, r.Data[0].Versions, 2)
	require.Equal(t, "shared.lib", r.Data[0].ID)
	w := f.requestRepo(t, "GET", "all", "/v3/registration-semver2/shared.lib/index.json", "admin")
	require.Equal(t, 200, w.Code, w.Body.String())
	gz, e := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, e)
	b, e := io.ReadAll(gz)
	require.NoError(t, e)
	gz.Close()
	require.Contains(t, string(b), "first member")
	require.NotContains(t, string(b), "second member")
	for _, p := range r.Data {
		for _, v := range p.Versions {
			path := strings.TrimPrefix(v.ID, "https://packages.example.test/root/repository/all")
			w = f.requestRepo(t, "GET", "all", path, "admin")
			require.Equal(t, 200, w.Code, w.Body.String())
			gz, e = gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
			require.NoError(t, e)
			b, e = io.ReadAll(gz)
			require.NoError(t, e)
			gz.Close()
			var leaf map[string]any
			require.NoError(t, json.Unmarshal(b, &leaf))
			path = strings.TrimPrefix(leaf["packageContent"].(string), "https://packages.example.test/root/repository/all")
			w = f.requestRepo(t, "GET", "all", path, "admin")
			require.Equal(t, 200, w.Code, w.Body.String())
			if p.ID == "shared.lib" && strings.HasPrefix(v.Version, "1.0.0") {
				require.Equal(t, winner, w.Body.Bytes())
			}
		}
	}
	w = f.requestRepo(t, "GET", "all", "/v3/flatcontainer/shared.lib/index.json", "admin")
	require.JSONEq(t, `{"versions":["1.0.0","2.0.0"]}`, w.Body.String())
	for _, p := range []string{"/v3/registration/missing/index.json", "/v3/registration/shared.lib/9.0.0.json"} {
		require.Equal(t, 404, f.requestRepo(t, "GET", "all", p, "admin").Code)
	}
	// Failure to fetch the selected bytes must never return the lower-priority copy.
	_, e = f.db.Exec(context.Background(), `UPDATE assets SET blob_key='missing' WHERE repository_id=$1`, f.repo.ID)
	require.NoError(t, e)
	require.Equal(t, 503, f.requestRepo(t, "GET", "all", "/v3/flatcontainer/shared.lib/1.0.0/shared.lib.1.0.0.nupkg", "admin").Code)
}
func TestGroupGlobalPagingAndCompleteVersions(t *testing.T) {
	f := hosted(t)
	f.addRepo(t, "second", domain.TypeHosted)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "second")
	for i := 0; i < 510; i++ {
		repo := "hosted"
		if i%2 == 1 {
			repo = "second"
		}
		f.pushTo(t, repo, fmt.Sprintf("pkg%03d", i), "1.0.0", "")
		f.pushTo(t, repo, "many", fmt.Sprintf("1.0.%d", i), "")
	}
	r := f.groupSearch(t, "all", "?skip=500&take=10", "admin")
	require.Equal(t, 511, r.TotalHits)
	require.Len(t, r.Data, 10)
	require.Equal(t, "pkg499", r.Data[0].ID)
	require.Equal(t, "pkg508", r.Data[9].ID)
	require.Equal(t, r, f.groupSearch(t, "all", "?skip=500&take=10", "admin"))
	r = f.groupSearch(t, "all", "?skip=511", "admin")
	require.Equal(t, 511, r.TotalHits)
	require.Empty(t, r.Data)
	r = f.groupSearch(t, "all", "?q=many", "admin")
	require.Len(t, r.Data[0].Versions, 510)
	w := f.requestRepo(t, "GET", "all", "/v3/registration/many/index.json", "admin")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "1.0.509")
	var doc struct {
		Items []struct {
			Count int
			Items []json.RawMessage
		}
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
	require.Equal(t, 510, doc.Items[0].Count)
	require.Len(t, doc.Items[0].Items, 510)
}
func TestGroupMembershipFailuresAndProxyExclusion(t *testing.T) {
	f := hosted(t)
	feed := newRemoteFeed(t, map[string][]remoteTestVersion{"remote": {{"1.0.0", "", true, 1, []byte("remote-package")}}})
	f.addRemote(t, "proxy", feed)
	f.addRepo(t, "nested-hosted", domain.TypeHosted)
	f.pushTo(t, "nested-hosted", "nested.secret", "1.0.0", "")
	f.addRepo(t, "nested", domain.TypeGroup, "nested-hosted")
	offline := f.addRepo(t, "offline", domain.TypeHosted)
	f.pushTo(t, "offline", "offline.secret", "1.0.0", "")
	offline.Online = false
	require.NoError(t, f.deps.Repos.Update(context.Background(), offline))
	other := f.addRepo(t, "other", domain.TypeHosted)
	f.pushTo(t, "other", "other.secret", "1.0.0", "")
	_, e := f.db.Exec(context.Background(), `UPDATE repositories SET format='raw' WHERE id=$1`, other.ID)
	require.NoError(t, e)
	g := f.addRepo(t, "all", domain.TypeGroup, "nested", "offline", "other", "hosted", "proxy")
	f.push(t, "local", "1.0.0")
	require.Equal(t, 1, f.groupSearch(t, "all", "?q=local", "admin").TotalHits)
	calls := feed.calls("/find")
	indexCalls := feed.calls("/feed/index.json")
	w := f.requestRepo(t, "GET", "all", "/index.json", "admin")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "/repository/all/v3/query")
	require.Contains(t, w.Body.String(), "RegistrationsBaseUrl/3.6.0")
	require.Equal(t, calls, feed.calls("/find"))
	require.Equal(t, indexCalls, feed.calls("/feed/index.json"))
	w = f.requestRepo(t, "HEAD", "all", "/index.json", "admin")
	require.Equal(t, 200, w.Code)
	require.Empty(t, w.Body.Bytes())
	f.addRepo(t, "local-only", domain.TypeGroup, "nested", "offline", "other", "hosted")
	for _, id := range []string{"nested.secret", "offline.secret", "other.secret"} {
		require.Equal(t, 404, f.requestRepo(t, "GET", "local-only", "/v3/flatcontainer/"+id+"/1.0.0/"+id+".1.0.0.nupkg", "admin").Code)
	}
	f.push(t, "sem2", "1.0.0+meta")
	require.Equal(t, 404, f.requestRepo(t, "GET", "all", "/v3/registration/sem2/index.json", "admin").Code)
	w = f.requestRepo(t, "GET", "all", "/v3/flatcontainer/remote/1.0.0/remote.1.0.0.nupkg", "admin")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "remote-package", w.Body.String())
	require.Equal(t, 1, f.groupSearch(t, "all", "?q=local", "admin").TotalHits)
	g.FormatConfig["member_names"] = []string{"proxy", "hosted"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	w = f.requestRepo(t, "GET", "all", "/v3/query", "admin")
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "unsupported_member_order")
	g.FormatConfig["member_names"] = []string{"hosted", "missing"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	require.Equal(t, 503, f.requestRepo(t, "GET", "all", "/v3/query", "admin").Code)
	g.FormatConfig["member_names"] = []string{"hosted"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	_, e = f.db.Exec(context.Background(), `UPDATE components SET extra='{}'`)
	require.NoError(t, e)
	require.Equal(t, 503, f.requestRepo(t, "GET", "all", "/v3/query", "admin").Code)
}
func TestGroupAuthorizationAndPublicBoundary(t *testing.T) {
	f := hosted(t)
	f.addRepo(t, "public", domain.TypeHosted)
	f.addRepo(t, "private", domain.TypeGroup, "hosted", "public")
	g := f.addRepo(t, "public-group", domain.TypeGroup, "public")
	g.AllowAnonymous = true
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	f.push(t, "secret", "1.0.0")
	f.pushTo(t, "public", "visible", "1.0.0", "")
	f.pushTo(t, "public", "visible", "2.0.0", "")
	r := f.groupSearch(t, "public-group", "", "anonymous")
	require.Equal(t, 1, r.TotalHits)
	require.Equal(t, "visible", r.Data[0].ID)
	require.Equal(t, 401, f.requestRepo(t, "GET", "private", "/v3/query", "anonymous").Code)
	require.Equal(t, 401, f.requestRepo(t, "GET", "public", "/v3/query", "anonymous").Code)
	_, e := f.db.Exec(context.Background(), `INSERT INTO users(id,username,email) VALUES('00000000-0000-0000-0000-000000000001','reader','reader@example.test'); INSERT INTO roles(id,name) VALUES('00000000-0000-0000-0000-000000000002','reader'); INSERT INTO user_roles VALUES('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002');`)
	require.NoError(t, e)
	grant := func(expr string) {
		_, e := f.db.Exec(context.Background(), `WITH s AS (INSERT INTO content_selectors(name,expression) VALUES($1,$2) RETURNING id), p AS (INSERT INTO privileges(name,type,content_selector_id,attrs) SELECT $1,'repository-content-selector',id,'{"actions":["read"]}' FROM s RETURNING id) INSERT INTO role_privileges SELECT '00000000-0000-0000-0000-000000000002',id FROM p`, uuid.NewString(), expr)
		require.NoError(t, e)
	}
	grant(`repository == "private" && path.startsWith("/v3/query")`)
	require.Zero(t, f.groupSearch(t, "private", "", "reader").TotalHits)
	grant(`repository == "private" && path.startsWith("/v3/flatcontainer/visible/index.json")`)
	grant(`repository == "private" && path.startsWith("/v3/flatcontainer/visible/1.0.0/")`)
	grant(`repository == "private" && path.startsWith("/v3/registration/visible/")`)
	r = f.groupSearch(t, "private", "", "reader")
	require.Equal(t, 1, r.TotalHits)
	require.Len(t, r.Data[0].Versions, 1)
	require.Equal(t, "1.0.0", r.Data[0].Version)
	require.Equal(t, 403, f.requestRepo(t, "GET", "public", "/v3/query", "reader").Code)
	require.Equal(t, 404, f.requestRepo(t, "GET", "private", "/v3/registration/visible/2.0.0.json", "reader").Code)
	_, e = f.db.Exec(context.Background(), `DELETE FROM role_privileges`)
	require.NoError(t, e)
	grant(`repository == "public"`)
	require.Equal(t, 403, f.requestRepo(t, "GET", "private", "/v3/query", "reader").Code)
}

func TestGroupRoutingFiltersBeforeCountingAndFailsClosed(t *testing.T) {
	f := hosted(t)
	g := f.addRepo(t, "all", domain.TypeGroup, "hosted")
	f.push(t, "allowed", "1.0.0")
	f.push(t, "allowed", "2.0.0")
	f.push(t, "secret", "1.0.0")
	rule := &domain.RoutingRule{Name: uuid.NewString(), Mode: "BLOCK", Matchers: []string{`/secret/`, `/allowed/2\.0\.0/`}}
	require.NoError(t, f.deps.RoutingRules.Create(context.Background(), rule))
	g.RoutingRuleID = &rule.ID
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	r := f.groupSearch(t, "all", "?take=1", "admin")
	require.Equal(t, 1, r.TotalHits)
	require.Len(t, r.Data[0].Versions, 1)
	require.Equal(t, "1.0.0", r.Data[0].Version)
	w := f.requestRepo(t, "GET", "all", "/v3/flatcontainer/allowed/index.json", "admin")
	require.JSONEq(t, `{"versions":["1.0.0"]}`, w.Body.String())
	require.Equal(t, 404, f.requestRepo(t, "GET", "all", "/v3/registration/allowed/2.0.0.json", "admin").Code)
	require.Equal(t, 403, f.requestRepo(t, "GET", "all", "/v3/flatcontainer/allowed/2.0.0/allowed.2.0.0.nupkg", "admin").Code)
	rule.Matchers = []string{`/v3/query`}
	require.NoError(t, f.deps.RoutingRules.Update(context.Background(), rule))
	require.Equal(t, 403, f.requestRepo(t, "GET", "all", "/v3/query", "admin").Code)

}

func TestGroupEmptyIndexAndUnlistedPriority(t *testing.T) {
	f := hosted(t)
	f.addRepo(t, "empty-group", domain.TypeGroup)
	w := f.requestRepo(t, "GET", "empty-group", "/index.json", "admin")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "/repository/empty-group/v3/query")
	require.Zero(t, f.groupSearch(t, "empty-group", "", "admin").TotalHits)
	f.addRepo(t, "second", domain.TypeHosted)
	g := f.addRepo(t, "all", domain.TypeGroup, "hosted", "second")
	first := f.pushTo(t, "hosted", "same", "1.0.0", "first")
	second := f.pushTo(t, "second", "SAME", "1.0.0.0", "second")
	_, e := f.db.Exec(context.Background(), `UPDATE components SET extra=jsonb_set(extra,'{nuget,listed}','false') WHERE repository_id=$1`, f.repo.ID)
	require.NoError(t, e)
	require.Zero(t, f.groupSearch(t, "all", "", "admin").TotalHits)
	w = f.requestRepo(t, "GET", "all", "/v3/registration/same/1.0.0.json", "admin")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"listed":false`)
	w = f.requestRepo(t, "GET", "all", "/v3/flatcontainer/same/1.0.0/same.1.0.0.nupkg", "admin")
	require.Equal(t, first, w.Body.Bytes())
	g.FormatConfig["member_names"] = []string{"second", "hosted"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	require.Equal(t, 1, f.groupSearch(t, "all", "", "admin").TotalHits)
	w = f.requestRepo(t, "GET", "all", "/v3/flatcontainer/same/1.0.0/same.1.0.0.nupkg", "admin")
	require.Equal(t, second, w.Body.Bytes())
}

func TestGroupRemoteRegistrationUsesDiscoveredResources(t *testing.T) {
	f := hosted(t)
	feed := newRemoteFeed(t, map[string][]remoteTestVersion{"remote": {{"1.0.0", "remote", true, 1, []byte("remote bytes")}}})
	f.addRemote(t, "proxy", feed)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	leaf := responseJSON(t, f.requestRepo(t, "GET", "all", "/v3/registration-semver2/remote/1.0.0.json", "admin"))
	require.Equal(t, "https://packages.example.test/root/repository/all/v3/registration-semver2/remote/index.json", leaf["registration"])
	require.Equal(t, "https://packages.example.test/root/repository/all/v3/registration-semver2/remote/1.0.0.json", leaf["@id"])
	path := strings.TrimPrefix(leaf["packageContent"].(string), "https://packages.example.test/root/repository/all")
	w := f.requestRepo(t, "GET", "all", path, "admin")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "remote bytes", w.Body.String())
	require.Equal(t, 1, f.groupSearch(t, "all", "", "admin").TotalHits)
	feed.server.Close()
	w = f.requestRepo(t, "GET", "all", path, "admin")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "remote bytes", w.Body.String())
	require.Equal(t, 1, f.groupSearch(t, "all", "", "admin").TotalHits)
}
