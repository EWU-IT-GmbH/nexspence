//go:build integration

package nuget_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexspence-oss/nexspence/internal/api/handlers"
	"github.com/nexspence-oss/nexspence/internal/auth"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/group"
	"github.com/nexspence-oss/nexspence/internal/formats/nuget"
	"github.com/nexspence-oss/nexspence/internal/logger"
	"github.com/nexspence-oss/nexspence/internal/repository/postgres"
	"github.com/nexspence-oss/nexspence/internal/service"
	"github.com/stretchr/testify/require"
)

type clientFixture struct {
	hostedFixture
	server                      *httptest.Server
	reader, writer, writeScoped string
}

// Real PostgreSQL users, token hashes, Auth/RBAC middleware and protocol handlers.
// Only repository seeding and blob storage are fixtures; there is no X-Test-Actor.
func authenticatedFeed(t *testing.T) clientFixture {
	t.Helper()
	f := hosted(t)
	f.addRepo(t, "public", domain.TypeHosted)
	f.addRepo(t, "nuget", domain.TypeGroup, "hosted", "public")
	public := f.addRepo(t, "nuget-public", domain.TypeGroup, "public")
	public.AllowAnonymous = true
	require.NoError(t, f.deps.Repos.Update(context.Background(), public))
	f.pushTo(t, "hosted", "ewu.trace", "1.9.0", "Private trace library")
	f.pushTo(t, "hosted", "ewu.trace", "1.10.0", "Private trace library")
	f.pushTo(t, "hosted", "ewu.trace", "2.0.0-beta.2", "Private preview")
	f.pushTo(t, "hosted", "ewu.trace.tools", "1.0.0", "Private tools")
	f.pushTo(t, "public", "ewu.public", "1.0.0", "Public example")
	ctx := context.Background()
	users := postgres.NewUserRepo(f.db)
	tokens := service.NewTokenService(postgres.NewUserTokenRepo(f.db), users)
	authSvc := auth.NewService(uuid.NewString(), 1, 4)
	log := logger.New("error", "json")
	userSvc := service.NewUserService(users, postgres.NewRoleRepo(f.db), authSvc, log)
	create := func(name string, scopes []string, repos ...string) string {
		u := &domain.User{Username: name, Status: domain.UserStatusActive, Source: domain.UserSourceLocal}
		require.NoError(t, users.Create(ctx, u))
		role := uuid.NewString()
		_, err := f.db.Exec(ctx, `INSERT INTO roles(id,name) VALUES($1,$2)`, role, name)
		require.NoError(t, err)
		_, err = f.db.Exec(ctx, `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, u.ID, role)
		require.NoError(t, err)
		for _, repo := range repos {
			_, err = f.db.Exec(ctx, `WITH s AS (INSERT INTO content_selectors(name,expression) VALUES($1,$2) RETURNING id), p AS (INSERT INTO privileges(name,type,content_selector_id,attrs) SELECT $1,'repository-content-selector',id,'{"actions":["read","write"]}' FROM s RETURNING id) INSERT INTO role_privileges SELECT $3,id FROM p`, uuid.NewString(), `repository == "`+repo+`"`, role)
			require.NoError(t, err)
		}
		tok, err := tokens.Create(ctx, u.ID, "local acceptance", scopes, nil)
		require.NoError(t, err)
		return tok.Token
	}
	out := clientFixture{hostedFixture: f}
	out.reader = create("reader", []string{"read"}, "nuget")
	out.writer = create("ci", []string{"read", "write"}, "hosted", "nuget")
	out.writeScoped = create("write-only", []string{"write"}, "nuget")
	server := httptest.NewUnstartedServer(nil)
	deps := f.deps
	deps.BaseURL = "http://" + server.Listener.Addr().String()
	h := nuget.New(deps)
	gh := group.New(deps, map[string]formats.FormatHandler{"nuget": h})
	router := gin.New()
	router.Any("/repository/:repoName/*path", handlers.OptionalAuth(userSvc, tokens, nil, log), handlers.RBACMiddleware(deps.RBAC.(*service.RBACService), deps.Repos), func(c *gin.Context) {
		repo, err := deps.Repos.Get(c.Request.Context(), c.Param("repoName"))
		if err != nil || repo == nil {
			c.Status(404)
			return
		}
		if repo.Type == domain.TypeGroup {
			gh.ServeHTTP(c)
		} else {
			h.ServeHTTP(c)
		}
	})
	server.Config.Handler = router
	server.Start()
	t.Cleanup(server.Close)
	out.server = server
	return out
}

func TestSearchRealAPITokenAuthentication(t *testing.T) {
	f := authenticatedFeed(t)
	request := func(path, token, mode string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			if mode == "basic" {
				r.SetBasicAuth("reader", token)
			} else {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}
		w := httptest.NewRecorder()
		f.server.Config.Handler.ServeHTTP(w, r)
		return w
	}
	for _, mode := range []string{"basic", "bearer"} {
		w := request("/repository/nuget/v3/query?q=ewu.trace", f.reader, mode)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"version":"1.10.0"`)
		require.NotContains(t, w.Body.String(), "beta")
		require.Equal(t, 403, request("/repository/hosted/v3/query", f.reader, mode).Code)
		// Existing scope contract: write includes read.
		require.Equal(t, 200, request("/repository/nuget/v3/query", f.writeScoped, mode).Code)
	}
	for _, token := range []string{"", "nxs_invalid"} {
		require.Equal(t, 401, request("/repository/nuget/v3/query", token, "basic").Code)
		w := request("/repository/nuget-public/v3/query", token, "basic")
		require.Equal(t, 200, w.Code)
		require.Contains(t, w.Body.String(), "ewu.public")
		require.NotContains(t, w.Body.String(), "ewu.trace")
		require.NotContains(t, w.Body.String(), "Private")
		require.NotContains(t, w.Body.String(), "1.10.0")
	}
	// A read token must not reach the mutation handler, even on an allowed repo.
	r := httptest.NewRequest("PUT", "/repository/nuget/api/v2/package", nil)
	r.SetBasicAuth("reader", f.reader)
	w := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(w, r)
	require.Equal(t, 403, w.Code)
	// A real token must also respect a content-selector change immediately.
	_, err := f.db.Exec(context.Background(), `UPDATE content_selectors SET expression='repository == "nuget" && path.startsWith("/v3/query")' WHERE id IN (SELECT p.content_selector_id FROM privileges p JOIN role_privileges rp ON rp.privilege_id=p.id JOIN roles r ON r.id=rp.role_id WHERE r.name='reader')`)
	require.NoError(t, err)
	w = request("/repository/nuget/v3/query", f.reader, "basic")
	require.Equal(t, 200, w.Code)
	require.JSONEq(t, `{"totalHits":0,"data":[]}`, w.Body.String())
	// Revocation must affect the very next search, including a previously used token.
	_, err = f.db.Exec(context.Background(), `DELETE FROM user_tokens WHERE user_id IN (SELECT id FROM users WHERE username='reader')`)
	require.NoError(t, err)
	require.Equal(t, 401, request("/repository/nuget/v3/query", f.reader, "basic").Code)
}

// Serves the same authenticated feed to a real host .NET/Rider client. Opt-in,
// loopback only, bounded lifetime, isolated DB. The Python runner signals cleanup.
func TestLocalNuGetClientGate(t *testing.T) {
	dir := os.Getenv("NEXSPENCE_LOCAL_NUGET_DIR")
	if dir == "" {
		t.Skip("opt-in local client acceptance")
	}
	f := authenticatedFeed(t)
	b, err := json.Marshal(map[string]string{"url": f.server.URL, "reader": f.reader, "writer": f.writer})
	require.NoError(t, err)
	// Parent directory is private (0700); permit its host owner to read the
	// Docker-root-created readiness file without exposing it outside that directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready.tmp"), b, 0644))
	require.NoError(t, os.Rename(filepath.Join(dir, "ready.tmp"), filepath.Join(dir, "ready.json")))
	wait := 8 * time.Minute
	if value := os.Getenv("NEXSPENCE_LOCAL_NUGET_WAIT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, seconds, 180)
		require.LessOrEqual(t, seconds, 3780)
		wait = time.Duration(seconds) * time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("client acceptance timed out")
		case <-ticker.C:
			b, err := os.ReadFile(filepath.Join(dir, "done.json"))
			if os.IsNotExist(err) {
				continue
			}
			require.NoError(t, err)
			require.Equal(t, "ok", strings.TrimSpace(string(b)))
			asset, err := f.deps.Assets.GetByPath(context.Background(), "hosted", "/ewu.ci/1.0.0/ewu.ci.1.0.0.nupkg")
			require.NoError(t, err)
			require.Positive(t, asset.SizeBytes)
			return
		}
	}
}
