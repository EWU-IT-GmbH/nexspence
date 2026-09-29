package nuget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/stretchr/testify/require"
)

func TestRemoteSearchRejectsIncompleteAndOversizedResults(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"broad", `{"totalHits":4001,"data":[]}`, "remote_search_too_broad"},
		{"missing-count", `{"data":[]}`, "invalid_upstream_response"},
		{"missing-data", `{"totalHits":0}`, "invalid_upstream_response"},
		{"empty-page", `{"totalHits":1,"data":[]}`, "incomplete_upstream_results"},
		{"missing-downloads", `{"totalHits":1,"data":[{"id":"pkg","version":"1.0.0","versions":[{"version":"1.0.0","@id":"https://example.test/leaf"}]}]}`, "invalid_upstream_response"},
		{"negative-downloads", `{"totalHits":1,"data":[{"id":"pkg","version":"1.0.0","versions":[{"version":"1.0.0","downloads":-1,"@id":"https://example.test/leaf"}]}]}`, "invalid_upstream_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var server *httptest.Server
			var calls atomic.Int32
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v3/index.json" {
					fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, server.URL+"/search")
					return
				}
				calls.Add(1)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			h := New(formats.Deps{})
			repo := &domain.Repository{Name: "proxy", ProxyConfig: map[string]any{"remote_url": server.URL}}
			for i := 0; i < 2; i++ {
				_, err := h.remoteSearch(context.Background(), repo, "caller", SearchOptions{Take: 20})
				require.ErrorContains(t, err, tc.code)
			}
			require.EqualValues(t, 2, calls.Load(), "failed collections must not enter the search cache")
		})
	}
}
func TestRemoteSearchDetectsChangingPagesAndEncodesParameters(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/index.json" {
			fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService","@id":%q}]}`, server.URL+"/search?tenant=one")
			return
		}
		if r.URL.Query().Get("q") != "a+b %_\\" || r.URL.Query().Get("tenant") != "one" || r.URL.Query().Get("prerelease") != "true" || r.URL.Query().Get("semVerLevel") != "2.0.0" || r.URL.Query().Get("take") != "1000" {
			t.Error("search parameters were changed")
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		total := 2
		if skip > 0 {
			total = 3
		}
		fmt.Fprintf(w, `{"totalHits":%d,"data":[{"id":"pkg","version":"1.0.0","versions":[{"version":"1.0.0","downloads":0,"@id":"https://example.test/leaf"}]}]}`, total)
	}))
	defer server.Close()
	h := New(formats.Deps{})
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": server.URL}}
	_, err := h.remoteSearch(context.Background(), repo, "caller", SearchOptions{Query: "a+b %_\\", Take: 1, Prerelease: true, SemVer2: true})
	require.ErrorContains(t, err, "upstream_results_changed")
}
func TestRemoteCacheTTLBoundsAndContextKeys(t *testing.T) {
	cache := newRemoteCache()
	now := time.Now()
	cache.now = func() time.Time { return now }
	cache.put("k", []byte("value"))
	b, ok := cache.get("k")
	require.True(t, ok)
	b[0] = 'X'
	b, _ = cache.get("k")
	require.Equal(t, "value", string(b))
	now = now.Add(remoteCacheTTL)
	_, ok = cache.get("k")
	require.False(t, ok)
	for i := 0; i < 130; i++ {
		cache.put(strconv.Itoa(i), []byte("x"))
		now = now.Add(time.Millisecond)
	}
	require.LessOrEqual(t, len(cache.entries), 128)
	cache.put("large", []byte(strings.Repeat("x", remoteCollectionBytes+1)))
	_, ok = cache.get("large")
	require.False(t, ok)
	repo := &domain.Repository{Name: "proxy", ProxyConfig: map[string]any{"remote_password": "secret"}}
	key := remoteKey("user-one", repo, "search", SearchOptions{Take: 20})
	require.NotContains(t, key, "secret")
	require.NotEqual(t, key, remoteKey("user-two", repo, "search", SearchOptions{Take: 20}))
	require.NotEqual(t, key, remoteKey("user-one", repo, "search", SearchOptions{Take: 20, Skip: 1}))
	repo.ProxyConfig["remote_password"] = "changed"
	require.NotEqual(t, key, remoteKey("user-one", repo, "search", SearchOptions{Take: 20}))
}
