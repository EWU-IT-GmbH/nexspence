package nuget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
	"github.com/nexspence-oss/nexspence/internal/netguard"
	"github.com/stretchr/testify/require"
)

func TestNuGetDiscoverSearchResourceAndRemoteAuthentication(t *testing.T) {
	var auth atomic.Int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "remote-user" || password != "remote-pass" || r.Header.Get("Cookie") != "" || r.Header.Get("X-NuGet-ApiKey") != "" {
			w.WriteHeader(401)
			return
		}
		auth.Add(1)
		w.Write([]byte(`{"totalHits":0,"data":[]}`))
	}))
	defer search.Close()
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/index.json" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"resources":[{"@type":"SearchQueryService/3.0.0-beta","@id":%q}]}`, search.URL+"/dynamic?tenant=test")
	}))
	defer index.Close()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": index.URL + "/custom/index.json", "remote_username": "remote-user", domain.RemotePasswordKey: "remote-pass"}}
	resource, err := discoverNuGetResource(context.Background(), repo, searchResourceTypes)
	require.NoError(t, err)
	require.Equal(t, search.URL+"/dynamic?tenant=test", resource)
	b, err := fetchNuGetJSON(context.Background(), repo, resource, 1024)
	require.NoError(t, err)
	require.JSONEq(t, `{"totalHits":0,"data":[]}`, string(b))
	require.EqualValues(t, 1, auth.Load())
}
func TestNuGetUpstreamLimitsAndInvalidResources(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.test/data", "/relative", "https://user:pass@example.test/query", "https://example.test/query#fragment"} {
		_, err := validateResourceURL(raw)
		require.Error(t, err, raw)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			w.Write([]byte(strings.Repeat("x", 1025)))
		case "/wait":
			<-r.Context().Done()
		default:
			w.WriteHeader(503)
		}
	}))
	defer upstream.Close()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": upstream.URL}}
	_, err := fetchNuGetJSON(context.Background(), repo, upstream.URL+"/large", 1024)
	require.ErrorContains(t, err, "result_too_large")
	_, err = fetchNuGetJSON(context.Background(), repo, upstream.URL+"/failure", 1024)
	require.ErrorContains(t, err, "upstream_unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = fetchNuGetJSON(ctx, repo, upstream.URL+"/wait", 1024)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}
func TestNuGetDynamicResourceUsesSSRFGuard(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("blocked address reached") }))
	defer upstream.Close()
	old := repoproxy.UpstreamClient
	repoproxy.UpstreamClient = netguard.Client(time.Second)
	defer func() { repoproxy.UpstreamClient = old }()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": "https://example.test"}}
	_, err := fetchNuGetJSON(context.Background(), repo, upstream.URL, 1024)
	require.ErrorContains(t, err, "blocked connection")
}
func TestNuGetUpstreamParallelismIsBounded(t *testing.T) {
	var active, peak atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	repo := &domain.Repository{ProxyConfig: map[string]any{"remote_url": upstream.URL}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fetchNuGetJSON(context.Background(), repo, upstream.URL, 1024)
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("requests did not start")
		}
	}
	select {
	case <-started:
		close(release)
		t.Fatal("more than four upstream requests")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 4, peak.Load())
}
