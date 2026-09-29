//go:build integration

package nuget_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/stretchr/testify/require"
)

type remoteTestVersion struct {
	version, description string
	listed               bool
	downloads            int64
	body                 []byte
}
type recordedRemoteRequest struct {
	path   string
	query  map[string][]string
	header http.Header
}
type remoteTestFeed struct {
	server   *httptest.Server
	packages map[string][]remoteTestVersion
	mu       sync.Mutex
	requests []recordedRemoteRequest
}

func newRemoteFeed(t *testing.T, packages map[string][]remoteTestVersion) *remoteTestFeed {
	t.Helper()
	feed := &remoteTestFeed{packages: packages}
	feed.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feed.mu.Lock()
		feed.requests = append(feed.requests, recordedRemoteRequest{r.URL.Path, r.URL.Query(), r.Header.Clone()})
		feed.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		if r.URL.Path == "/feed/index.json" {
			write(map[string]any{"version": "3.0.0", "resources": []map[string]string{
				{"@type": "SearchQueryService/3.0.0-rc", "@id": feed.server.URL + "/find?tenant=preserved"},
				{"@type": "RegistrationsBaseUrl/3.6.0", "@id": feed.server.URL + "/metadata/"},
				{"@type": "PackageBaseAddress/3.0.0", "@id": feed.server.URL + "/content/"},
			}})
			return
		}
		if r.URL.Path == "/find" {
			q := strings.ToLower(r.URL.Query().Get("q"))
			prerelease := r.URL.Query().Get("prerelease") == "true"
			sem2 := r.URL.Query().Get("semVerLevel") == "2.0.0"
			ids := []string{}
			for id := range packages {
				ids = append(ids, id)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(ids)))
			data := []map[string]any{}
			for _, id := range ids {
				versions := append([]remoteTestVersion(nil), packages[id]...)
				sort.Slice(versions, func(i, j int) bool {
					a, _ := nugetmeta.ParseVersion(versions[i].version)
					b, _ := nugetmeta.ParseVersion(versions[j].version)
					return a.Compare(b) < 0
				})
				matched := strings.Contains(strings.ToLower(id), q)
				for _, v := range versions {
					matched = matched || strings.Contains(strings.ToLower(v.description), q)
				}
				if strings.HasPrefix(q, "packageid:") {
					matched = strings.EqualFold(id, strings.TrimPrefix(q, "packageid:"))
				}
				if !matched {
					continue
				}
				vs := []map[string]any{}
				var latest remoteTestVersion
				for _, version := range versions {
					v, _ := nugetmeta.ParseVersion(version.version)
					if !version.listed || (!prerelease && v.Release != "") || (!sem2 && v.SemVer2()) {
						continue
					}
					latest = version
					vs = append(vs, map[string]any{"version": version.version, "downloads": version.downloads, "@id": feed.server.URL + "/metadata/" + strings.ToLower(id) + "/" + v.Key() + ".json"})
				}
				if len(vs) == 0 {
					continue
				}
				data = append(data, map[string]any{"id": id, "version": latest.version, "description": latest.description, "authors": []string{"Remote author"}, "tags": []string{"remote", "test"}, "versions": vs})
			}
			skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
			take, _ := strconv.Atoi(r.URL.Query().Get("take"))
			if take == 0 {
				take = 20
			}
			total := len(data)
			if skip > total {
				skip = total
			}
			end := skip + take
			if end > total {
				end = total
			}
			write(map[string]any{"totalHits": total, "data": data[skip:end]})
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			w.WriteHeader(404)
			return
		}
		id := parts[1]
		var versions []remoteTestVersion
		for name, vs := range packages {
			if strings.EqualFold(name, id) {
				versions = vs
				break
			}
		}
		if versions == nil {
			w.WriteHeader(404)
			return
		}
		if parts[0] == "metadata" && parts[2] == "index.json" {
			items := []map[string]any{}
			for _, version := range versions {
				v, _ := nugetmeta.ParseVersion(version.version)
				items = append(items, map[string]any{"@id": feed.server.URL + "/metadata/" + id + "/" + v.Key() + ".json", "packageContent": feed.server.URL + "/content/" + id + "/" + v.Key() + "/" + id + "." + v.Key() + ".nupkg", "catalogEntry": map[string]any{"id": id, "version": version.version, "description": version.description, "authors": []string{"Remote author"}, "published": "2026-01-01T00:00:00Z", "listed": version.listed}})
			}
			write(map[string]any{"count": 1, "items": []map[string]any{{"count": len(items), "items": items}}})
			return
		}
		if parts[0] == "content" && parts[2] == "index.json" {
			keys := []string{}
			for _, version := range versions {
				v, _ := nugetmeta.ParseVersion(version.version)
				keys = append(keys, v.Key())
			}
			write(map[string]any{"versions": keys})
			return
		}
		if parts[0] == "content" && len(parts) == 4 {
			for _, version := range versions {
				v, _ := nugetmeta.ParseVersion(version.version)
				if v.Key() == parts[2] && version.body != nil {
					w.Header().Set("Content-Type", "application/zip")
					_, _ = w.Write(version.body)
					return
				}
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(feed.server.Close)
	return feed
}
func (f *remoteTestFeed) calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.path == path {
			n++
		}
	}
	return n
}
func (f hostedFixture) addRemote(t *testing.T, name string, feed *remoteTestFeed) *domain.Repository {
	t.Helper()
	repo := f.addRepo(t, name, domain.TypeProxy)
	repo.ProxyConfig = map[string]any{"remote_url": feed.server.URL + "/feed/index.json"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), repo))
	return repo
}
func responseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, 200, w.Code, w.Body.String())
	b := w.Body.Bytes()
	if w.Header().Get("Content-Encoding") == "gzip" {
		gz, e := gzip.NewReader(bytes.NewReader(b))
		require.NoError(t, e)
		b, e = io.ReadAll(gz)
		require.NoError(t, e)
		gz.Close()
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}
func TestFederatedSearchLocalRemoteOverlapAndRestore(t *testing.T) {
	f := hosted(t)
	local := f.pushTo(t, "hosted", "Alpha", "1.0.0", "local winner")
	remoteBytes := buildNupkgWithManifest(t, `<package><metadata><id>Alpha</id><version>2.0.0</version><description>remote latest</description></metadata></package>`)
	feed := newRemoteFeed(t, map[string][]remoteTestVersion{
		"ALPHA": {{"1.0.0.0", "remote loser", true, 11, []byte("wrong bytes")}, {"2.0.0", "remote latest", true, 22, remoteBytes}},
		"zeta":  {{"1.0.0", "remote only", true, 33, []byte("zeta bytes")}},
	})
	f.addRemote(t, "proxy", feed)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	r := f.groupSearch(t, "all", "", "admin")
	require.Equal(t, 2, r.TotalHits)
	require.Equal(t, "alpha", r.Data[0].ID)
	require.Equal(t, "2.0.0", r.Data[0].Version)
	require.Equal(t, "remote latest", r.Data[0].Description)
	require.Len(t, r.Data[0].Versions, 2)
	require.EqualValues(t, 0, r.Data[0].Versions[0].Downloads)
	require.EqualValues(t, 22, r.Data[0].Versions[1].Downloads)
	for _, pkg := range r.Data {
		for _, version := range pkg.Versions {
			leaf := responseJSON(t, f.requestRepo(t, "GET", "all", strings.TrimPrefix(version.ID, "https://packages.example.test/root/repository/all"), "admin"))
			path := strings.TrimPrefix(leaf["packageContent"].(string), "https://packages.example.test/root/repository/all")
			w := f.requestRepo(t, "GET", "all", path, "admin")
			require.Equal(t, 200, w.Code, w.Body.String())
			if pkg.ID == "alpha" && version.Version == "1.0.0" {
				require.Equal(t, local, w.Body.Bytes())
			}
			if pkg.ID == "alpha" && version.Version == "2.0.0" {
				require.Equal(t, remoteBytes, w.Body.Bytes())
			}
		}
	}
	w := f.requestRepo(t, "GET", "all", "/v3/flatcontainer/alpha/index.json", "admin")
	require.JSONEq(t, `{"versions":["1.0.0","2.0.0"]}`, w.Body.String())
	r = f.groupSearch(t, "all", "?skip=1&take=1", "admin")
	require.Equal(t, 2, r.TotalHits)
	require.Equal(t, "zeta", r.Data[0].ID)
	r = f.groupSearch(t, "all", "?skip=2", "admin")
	require.Equal(t, 2, r.TotalHits)
	require.Empty(t, r.Data)
}
func TestFederatedSearchMultipleRemotesRespectUnlistedPriority(t *testing.T) {
	f := hosted(t)
	first := newRemoteFeed(t, map[string][]remoteTestVersion{"shared": {{"1.0.0", "hidden winner", false, 5, []byte("unlisted first")}, {"2.0.0", "first winner", true, 7, []byte("first")}}})
	second := newRemoteFeed(t, map[string][]remoteTestVersion{"SHARED": {{"1.0.0.0", "listed loser", true, 500, []byte("second")}, {"2.0.0.0", "duplicate loser", true, 700, []byte("second")}, {"3.0.0", "second latest", true, 900, []byte("third")}}})
	f.addRemote(t, "first", first)
	f.addRemote(t, "second", second)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "first", "second")
	r := f.groupSearch(t, "all", "?q=shared", "admin")
	require.Equal(t, 1, r.TotalHits)
	require.Len(t, r.Data[0].Versions, 2)
	require.Equal(t, "2.0.0", r.Data[0].Versions[0].Version)
	require.EqualValues(t, 7, r.Data[0].Versions[0].Downloads)
	leaf := responseJSON(t, f.requestRepo(t, "GET", "all", "/v3/registration/shared/1.0.0.json", "admin"))
	require.Equal(t, false, leaf["listed"])
	w := f.requestRepo(t, "GET", "all", "/v3/flatcontainer/shared/2.0.0/shared.2.0.0.nupkg", "admin")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "first", w.Body.String())
}
func TestFederatedSearchFourThousandGlobalResults(t *testing.T) {
	f := hosted(t)
	packages := map[string][]remoteTestVersion{}
	for i := 0; i < 4000; i++ {
		packages[fmt.Sprintf("pkg%04d", i)] = []remoteTestVersion{{"1.0.0", "", true, 1, nil}}
	}
	feed := newRemoteFeed(t, packages)
	f.addRemote(t, "proxy", feed)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	f.push(t, "pkg0000", "1.0.0")
	f.push(t, "local-only", "1.0.0")
	r := f.groupSearch(t, "all", "?skip=3999&take=2", "admin")
	require.Equal(t, 4001, r.TotalHits)
	require.Len(t, r.Data, 2)
	require.Equal(t, "pkg3998", r.Data[0].ID)
	require.Equal(t, "pkg3999", r.Data[1].ID)
	require.Equal(t, 4, feed.calls("/find"))
	require.Equal(t, r, f.groupSearch(t, "all", "?skip=3999&take=2", "admin"))
	require.Equal(t, 4, feed.calls("/find"))
}

func TestFederatedSearchCacheRechecksPermissionsAndDoesNotForwardCredentials(t *testing.T) {
	f := hosted(t)
	feed := newRemoteFeed(t, map[string][]remoteTestVersion{
		"public": {{"1.0.0", "visible old", true, 1, []byte("old")}, {"2.0.0", "hidden latest", true, 2, []byte("latest")}},
		"secret": {{"1.0.0", "secret description", true, 9, nil}},
	})
	proxy := f.addRemote(t, "proxy", feed)
	proxy.ProxyConfig["remote_username"] = "remote-user"
	proxy.ProxyConfig[domain.RemotePasswordKey] = "remote-password"
	require.NoError(t, f.deps.Repos.Update(context.Background(), proxy))
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	_, e := f.db.Exec(context.Background(), `INSERT INTO users(id,username,email) VALUES('00000000-0000-0000-0000-000000000001','reader','reader@example.test'); INSERT INTO roles(id,name) VALUES('00000000-0000-0000-0000-000000000002','reader'); INSERT INTO user_roles VALUES('00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002');`)
	require.NoError(t, e)
	grant := func(name, path string) {
		_, e := f.db.Exec(context.Background(), `WITH s AS (INSERT INTO content_selectors(name,expression) VALUES($1,$2) RETURNING id), p AS (INSERT INTO privileges(name,type,content_selector_id,attrs) SELECT $1,'repository-content-selector',id,'{"actions":["read"]}' FROM s RETURNING id) INSERT INTO role_privileges SELECT '00000000-0000-0000-0000-000000000002',id FROM p`, name, `repository == "all" && path.startsWith("`+path+`")`)
		require.NoError(t, e)
	}
	grant("query", "/v3/query")
	require.Zero(t, f.groupSearch(t, "all", "", "reader").TotalHits)
	require.Equal(t, 1, feed.calls("/find"))
	grant("flat-index", "/v3/flatcontainer/public/index.json")
	grant("content", "/v3/flatcontainer/public/1.0.0/")
	grant("registration", "/v3/registration/public/")
	r := f.groupSearch(t, "all", "", "reader")
	require.Equal(t, 1, r.TotalHits)
	require.Equal(t, "1.0.0", r.Data[0].Version)
	require.Equal(t, "visible old", r.Data[0].Description)
	require.Len(t, r.Data[0].Versions, 1)
	require.Equal(t, 1, feed.calls("/find"), "cached upstream data must be filtered with current permissions")
	req := httptest.NewRequest("GET", "/repository/all/v3/query?q=public", nil)
	req.Header.Set("Authorization", "Bearer nexspence-user-secret")
	req.Header.Set("Cookie", "session=nexspence-cookie")
	req.Header.Set("X-NuGet-ApiKey", "nexspence-api-key")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code, w.Body.String())
	feed.mu.Lock()
	requests := append([]recordedRemoteRequest(nil), feed.requests...)
	feed.mu.Unlock()
	for _, r := range requests {
		request := &http.Request{Header: r.header}
		user, password, ok := request.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "remote-user", user)
		require.Equal(t, "remote-password", password)
		require.Empty(t, r.header.Get("Cookie"))
		require.Empty(t, r.header.Get("X-NuGet-ApiKey"))
	}
	previous := feed.calls("/find")
	f.groupSearch(t, "all", "?q=public", "reader")
	require.Equal(t, previous+1, feed.calls("/find"), "caller context must not reuse admin cache entries")
	proxy.ProxyConfig["remote_username"] = "new-user"
	require.NoError(t, f.deps.Repos.Update(context.Background(), proxy))
	f.groupSearch(t, "all", "?q=public", "reader")
	require.Equal(t, previous+2, feed.calls("/find"))
}

func TestFederatedSearchFailsRatherThanReturningLocalPartialData(t *testing.T) {
	f := hosted(t)
	f.push(t, "local", "1.0.0")
	var remote *httptest.Server
	remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/index.json" {
			fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, remote.URL+"/search")
			return
		}
		w.Write([]byte(`{"totalHits":4001,"data":[]}`))
	}))
	defer remote.Close()
	proxy := f.addRepo(t, "proxy", domain.TypeProxy)
	proxy.ProxyConfig = map[string]any{"remote_url": remote.URL}
	require.NoError(t, f.deps.Repos.Update(context.Background(), proxy))
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	w := f.requestRepo(t, "GET", "all", "/v3/query", "admin")
	require.Equal(t, 503, w.Code)
	require.JSONEq(t, `{"error":"remote_search_too_broad"}`, w.Body.String())
	remote.Close()
	w = f.requestRepo(t, "GET", "all", "/v3/query", "admin")
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "local")
}

func TestFederatedRemoteFiltersAndUnavailableWinner(t *testing.T) {
	f := hosted(t)
	first := newRemoteFeed(t, map[string][]remoteTestVersion{"filters": {{"1.9.0", "old", true, 9, nil}, {"1.10.0", "stable", true, 10, nil}, {"2.0.0-beta.2", "preview", true, 20, nil}, {"3.0.0+build", "semver2", true, 30, nil}}})
	second := newRemoteFeed(t, map[string][]remoteTestVersion{"filters": {{"1.10.0", "wrong fallback", true, 99, []byte("wrong")}}})
	f.addRemote(t, "first", first)
	f.addRemote(t, "second", second)
	f.addRepo(t, "all", domain.TypeGroup, "hosted", "first", "second")
	r := f.groupSearch(t, "all", "?q=filters", "admin")
	require.Equal(t, "1.10.0", r.Data[0].Version)
	require.Len(t, r.Data[0].Versions, 2)
	r = f.groupSearch(t, "all", "?q=filters&prerelease=true&semVerLevel=2.0.0", "admin")
	require.Equal(t, "3.0.0+build", r.Data[0].Version)
	require.Len(t, r.Data[0].Versions, 4)
	w := f.requestRepo(t, "GET", "all", "/v3/flatcontainer/filters/1.10.0/filters.1.10.0.nupkg", "admin")
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "upstream_package_unavailable")
	require.Zero(t, second.calls("/content/filters/1.10.0/filters.1.10.0.nupkg"))
}
