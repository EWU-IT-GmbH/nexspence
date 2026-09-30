package nuget_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestDirectProxySearchUsesDiscoveredHost(t *testing.T) {
	calls := 0
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/custom/find", r.URL.Path)
		require.Equal(t, "fixed", r.URL.Query().Get("tenant"))
		require.Equal(t, "20", r.URL.Query().Get("skip"))
		require.Equal(t, "10", r.URL.Query().Get("take"))
		require.Equal(t, "true", r.URL.Query().Get("prerelease"))
		require.Equal(t, "2.0.0", r.URL.Query().Get("semVerLevel"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-NuGet-ApiKey"))
		user, password, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "remote", user)
		require.Equal(t, "secret", password)
		fmt.Fprintf(w, `{"totalHits":5000,"data":[{"id":%q,"version":"1.0.0"}]}`, r.URL.Query().Get("q"))
	}))
	defer search.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/custom/index.json", r.URL.Path)
		fmt.Fprintf(w, `{"version":"3.0.0","resources":[{"@type":"SearchQueryService/3.5.0","@id":%q}]}`, search.URL+"/custom/find?tenant=fixed")
	}))
	defer index.Close()
	repo := testutil.SimpleRepo("proxy", "nuget")
	repo.Type = domain.TypeProxy
	repo.ProxyConfig = map[string]any{"remote_url": index.URL + "/custom/index.json", "remote_username": "remote", domain.RemotePasswordKey: "secret"}
	router := setup(repo)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/repository/proxy/index.json", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `http://localhost:8080/repository/proxy/v3/query`)
	for _, term := range []string{"first", "second"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, "/repository/proxy/v3/query?q="+term+"&skip=20&take=10&prerelease=true&semVerLevel=2.0.0", nil)
			req.SetBasicAuth("caller", "do-not-forward")
			req.Header.Set("Cookie", "private=value")
			req.Header.Set("X-NuGet-ApiKey", "private")
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
			if method == http.MethodHead {
				require.Empty(t, w.Body.String())
			} else {
				require.Contains(t, w.Body.String(), `"id":"`+term+`"`)
				require.Contains(t, w.Body.String(), `"totalHits":5000`)
			}
		}
	}
	require.Equal(t, 4, calls)
}

func TestDirectProxySearchUpstreamFailure(t *testing.T) {
	for _, response := range []string{"unavailable", "invalid-json"} {
		t.Run(response, func(t *testing.T) {
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v3/index.json" {
					fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, upstream.URL+"/find")
				} else if response == "unavailable" {
					w.WriteHeader(502)
				} else {
					fmt.Fprint(w, "invalid")
				}
			}))
			defer upstream.Close()
			repo := testutil.SimpleRepo("proxy", "nuget")
			repo.Type = domain.TypeProxy
			repo.ProxyConfig = map[string]any{"remote_url": upstream.URL}
			router := setup(repo)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/repository/proxy/query?q=test", nil))
			require.Equal(t, 503, w.Code)
			require.NotContains(t, w.Body.String(), upstream.URL)
		})
	}
}

// Opt-in network check; ordinary test runs remain independent of nuget.org.
func TestDirectProxySearchLiveNuGetOrg(t *testing.T) {
	if os.Getenv("NUGET_LIVE_TEST") != "1" {
		t.Skip("set NUGET_LIVE_TEST=1 to query nuget.org")
	}
	repo := testutil.SimpleRepo("proxy", "nuget")
	repo.Type = domain.TypeProxy
	repo.ProxyConfig = map[string]any{"remote_url": "https://api.nuget.org/v3/index.json"}
	router := setup(repo)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/repository/proxy/v3/query?q=NuGet.Versioning&take=1&prerelease=false&semVerLevel=2.0.0", nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var result struct {
		TotalHits int `json:"totalHits"`
		Data      []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Positive(t, result.TotalHits)
	require.Len(t, result.Data, 1)
	require.Equal(t, "NuGet.Versioning", result.Data[0].ID)
}

func TestDirectProxyFullIndexURLResourcePaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v3-index/repository-signatures/5.0.0/index.json", r.URL.Path)
		fmt.Fprint(w, `{"allRepositorySigned":true}`)
	}))
	defer upstream.Close()
	repo := testutil.SimpleRepo("proxy", "nuget")
	repo.Type = domain.TypeProxy
	repo.ProxyConfig = map[string]any{"remote_url": upstream.URL + "/v3/index.json"}
	w := httptest.NewRecorder()
	setup(repo).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/repository/proxy/v3-index/repository-signatures/5.0.0/index.json", nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.JSONEq(t, `{"allRepositorySigned":true}`, w.Body.String())
}
