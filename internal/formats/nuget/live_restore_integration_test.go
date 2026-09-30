//go:build integration

package nuget_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/api/handlers"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/group"
	"github.com/nexspence-oss/nexspence/internal/formats/nuget"
	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
	"github.com/nexspence-oss/nexspence/internal/netguard"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
	"github.com/nexspence-oss/nexspence/internal/service"
	"github.com/stretchr/testify/require"
)

// Opt-in external acceptance gate: a host .NET SDK restores against this isolated
// anonymous feed. Ordinary tests remain deterministic and never contact nuget.org.
func TestFederatedLiveRestoreGate(t *testing.T) {
	dir := os.Getenv("NEXSPENCE_LIVE_NUGET_SMOKE_DIR")
	if dir == "" {
		t.Skip("opt-in live .NET restore acceptance")
	}
	f := hosted(t)
	proxy := f.addRepo(t, "proxy", domain.TypeProxy)
	proxy.ProxyConfig = map[string]any{"remote_url": "https://api.nuget.org/v3/index.json"}
	require.NoError(t, f.deps.Repos.Update(context.Background(), proxy))
	g := f.addRepo(t, "all", domain.TypeGroup, "hosted", "proxy")
	feed := "all"
	if os.Getenv("NEXSPENCE_LIVE_NUGET_DIRECT") == "1" {
		feed = "proxy"
		proxy.AllowAnonymous = true
		require.NoError(t, f.deps.Repos.Update(context.Background(), proxy))
	}
	g.AllowAnonymous = true
	require.NoError(t, f.deps.Repos.Update(context.Background(), g))
	old := repoproxy.UpstreamClient
	repoproxy.UpstreamClient = netguard.Client(30 * time.Second)
	defer func() { repoproxy.UpstreamClient = old }()
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	deps := f.deps
	deps.BaseURL = "http://" + server.Listener.Addr().String()
	h := nuget.New(deps)
	gh := group.New(deps, map[string]formats.FormatHandler{"nuget": h})
	router := gin.New()
	router.Any("/repository/:repoName/*path", handlers.RBACMiddleware(deps.RBAC.(*service.RBACService), deps.Repos), func(c *gin.Context) {
		if c.Param("repoName") == "proxy" {
			h.ServeHTTP(c)
		} else {
			gh.ServeHTTP(c)
		}
	})
	server.Config.Handler = router
	server.Start()
	response, err := http.Get(server.URL + "/repository/" + feed + "/v3/query?q=Nuget.Versioning&skip=0&take=300&prerelease=true&semVerLevel=2.0.0")
	require.NoError(t, err)
	defer response.Body.Close()
	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Equal(t, 200, response.StatusCode)
	version := ""
	for _, p := range result.Data {
		if strings.EqualFold(p.ID, "NuGet.Versioning") {
			if feed == "proxy" {
				require.Equal(t, "NuGet.Versioning", p.ID)
			}
			version = p.Version
			break
		}
	}
	require.NotEmpty(t, version)
	v, err := nugetmeta.ParseVersion(version)
	require.NoError(t, err)
	path := "/v3/flatcontainer/nuget.versioning/" + v.Key() + "/nuget.versioning." + v.Key() + ".nupkg"
	if feed == "proxy" {
		path = strings.Replace(path, "/v3/flatcontainer/", "/v3-flatcontainer/", 1)
	}
	_, err = f.deps.Assets.GetByPath(context.Background(), "proxy", path)
	require.Error(t, err, "package bytes must not already be cached")
	b, err := json.Marshal(map[string]string{"url": server.URL + "/repository/" + feed + "/index.json", "version": version})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready.tmp"), b, 0644))
	require.NoError(t, os.Rename(filepath.Join(dir, "ready.tmp"), filepath.Join(dir, "ready.json")))
	deadline := time.NewTimer(120 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("host .NET restore did not finish")
		case <-ticker.C:
			b, err := os.ReadFile(filepath.Join(dir, "done.json"))
			if os.IsNotExist(err) {
				continue
			}
			require.NoError(t, err)
			var done struct {
				ExitCode int `json:"exitCode"`
			}
			require.NoError(t, json.Unmarshal(b, &done))
			require.Zero(t, done.ExitCode)
			asset, err := f.deps.Assets.GetByPath(context.Background(), "proxy", path)
			require.NoError(t, err)
			require.Positive(t, asset.SizeBytes)
			t.Logf("Live search + clean .NET restore: NuGet.Versioning %s, cached bytes %d", version, asset.SizeBytes)
			return
		}
	}
}
