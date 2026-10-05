package nuget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
	"github.com/nexspence-oss/nexspence/internal/testutil"
	"github.com/stretchr/testify/require"
)

func authenticatedResource(w http.ResponseWriter, r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	if !ok || user != "remote" || password != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func TestRemoteSearchAuthWithCachedResource(t *testing.T) {
	var indexCalls, searchCalls atomic.Int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticatedResource(w, r) {
			return
		}
		searchCalls.Add(1)
		fmt.Fprint(w, `{"totalHits":0,"data":[]}`)
	}))
	defer search.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticatedResource(w, r) {
			return
		}
		indexCalls.Add(1)
		fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, search.URL+"/query")
	}))
	defer index.Close()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": index.URL, "remote_username": "remote", domain.RemotePasswordKey: "secret"}}
	h := New(formats.Deps{})
	for _, query := range []string{"one", "two"} {
		_, err := h.remoteSearch(context.Background(), repo, "caller", SearchOptions{Query: query, Take: 20})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, indexCalls.Load())
	require.EqualValues(t, 2, searchCalls.Load())
	// The same arbitrary target does not gain trust outside discovery's scope.
	_, err := fetchNuGetJSON(context.Background(), repo, search.URL+"/query", 1024)
	require.ErrorContains(t, err, "upstream_unavailable")
}

func TestRegistrationAuthScopeIncludesCachedRootButNotForeignPages(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials leaked to a registration page on another origin")
		}
		fmt.Fprint(w, `{"count":0,"items":[]}`)
	}))
	defer foreign.Close()
	var rootCalls, indexCalls atomic.Int32
	var root *httptest.Server
	root = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticatedResource(w, r) {
			return
		}
		rootCalls.Add(1)
		if r.URL.Path == "/page" {
			fmt.Fprint(w, `{"count":0,"items":[]}`)
			return
		}
		fmt.Fprintf(w, `{"count":2,"items":[{"@id":%q},{"@id":%q}]}`, root.URL+"/page", foreign.URL+"/page")
	}))
	defer root.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		indexCalls.Add(1)
		fmt.Fprintf(w, `{"resources":[{"@type":"RegistrationsBaseUrl/3.6.0","@id":%q}]}`, root.URL+"/registration/")
	}))
	defer index.Close()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": index.URL, "remote_username": "remote", domain.RemotePasswordKey: "secret"}}
	h := New(formats.Deps{})
	for _, id := range []string{"one", "two"} {
		_, err := h.remoteRegistration(context.Background(), repo, "caller", id)
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, indexCalls.Load())
	require.EqualValues(t, 4, rootCalls.Load())
	require.EqualValues(t, 2, foreignCalls.Load())
}

func TestDiscoveredResourceAuthDoesNotDowngradeHTTPS(t *testing.T) {
	var calls atomic.Int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent over HTTP from an HTTPS feed")
		}
		fmt.Fprint(w, `{"totalHits":0,"data":[]}`)
	}))
	defer search.Close()
	index := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticatedResource(w, r) {
			return
		}
		fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, search.URL)
	}))
	defer index.Close()
	old := repoproxy.UpstreamClient
	repoproxy.UpstreamClient = index.Client()
	defer func() { repoproxy.UpstreamClient = old }()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": index.URL, "remote_username": "remote", domain.RemotePasswordKey: "secret"}}
	_, err := New(formats.Deps{}).remoteSearch(context.Background(), repo, "caller", SearchOptions{Take: 20})
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestGroupProxyResourceAuthIsScopedToMemberAttempt(t *testing.T) {
	for _, tc := range []struct{ kind, path, body string }{
		{"PackageBaseAddress/3.0.0", "/v3/flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg", "package bytes"},
		{"RegistrationsBaseUrl/3.4.0", "/v3/registration/pkg/index.json", `{"count":0,"items":[]}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var calls atomic.Int32
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !authenticatedResource(w, r) {
					return
				}
				calls.Add(1)
				fmt.Fprint(w, tc.body)
			}))
			defer resource.Close()
			index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"resources":[{"@type":%q,"@id":%q}]}`, tc.kind, resource.URL+"/content/")
			}))
			defer index.Close()
			repo := testutil.SimpleRepo("proxy", "nuget")
			repo.Type = domain.TypeProxy
			repo.ProxyConfig = map[string]any{"remote_url": index.URL, "remote_username": "remote", domain.RemotePasswordKey: "secret"}
			h := New(formats.Deps{
				Repos: testutil.NewRepoRepo(repo), Blobs: testutil.NewBlobStoreRepo(),
				Components: testutil.NewComponentRepo(), Assets: testutil.NewAssetRepo(),
				BlobStore: testutil.NewBlobStore(), BaseURL: "https://packages.example.test",
			})
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			request := httptest.NewRequest(http.MethodGet, "/repository/group"+tc.path, nil)
			c.Request = request
			c.Params = gin.Params{{Key: "repoName", Value: repo.Name}, {Key: "path", Value: tc.path}}
			c.Set("nugetCallerRepository", "group")
			h.ServeHTTP(c)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.EqualValues(t, 1, calls.Load())
			require.Same(t, request, c.Request, "the next group member must not inherit trusted resource hosts")
			probe, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, resource.URL, nil)
			require.NoError(t, err)
			repoproxy.SetUpstreamAuth(probe, repo)
			require.Empty(t, probe.Header.Get("Authorization"))
		})
	}
}
