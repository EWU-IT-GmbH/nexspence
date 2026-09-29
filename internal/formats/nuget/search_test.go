package nuget

import (
	"context"
	"errors"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/repository"
	"github.com/stretchr/testify/require"
)

func TestSearchParameterContract(t *testing.T) {
	for _, raw := range []string{"q=", "q=%20%20", "q=%25_%5C", "skip=10000&take=100&prerelease=TRUE&semVerLevel=2.0.0", "packageType=&client=Rider"} {
		_, e := parseSearch(raw)
		require.NoError(t, e, raw)
	}
	for _, raw := range []string{"skip=-1", "skip=+1", "skip=1.0", "skip=10001", "skip=", "take=0", "take=101", "take=", "take=9999999999999999999999999999", "q=a&q=a", "semVerLevel=", "semVerLevel=2.0.0.0", "prerelease=", "prerelease=1", "packageType=Dependency", "q=%FF", "q=%00", "q=%XX", "q=a;b", "q=" + strings.Repeat("a", 257)} {
		_, e := parseSearch(raw)
		require.Error(t, e, raw)
		var q *queryError
		require.ErrorAs(t, e, &q)
		require.Equal(t, 400, q.status, raw)
	}
	_, e := parseSearch(strings.Repeat("a", 8193))
	var q *queryError
	require.ErrorAs(t, e, &q)
	require.Equal(t, 414, q.status)
	o, e := parseSearch("q=%2525&skip=001&take=02")
	require.NoError(t, e)
	require.Equal(t, "%25", o.Query)
	require.Equal(t, 1, o.Skip)
	require.Equal(t, 2, o.Take)
}
func TestSearchResponseLimitsAndHead(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/v3/query", nil)
		writeNuGetJSON(c, map[string]string{"data": strings.Repeat("x", maxSearchBytes)}, false)
		c.Writer.WriteHeaderNow()
		require.Equal(t, 503, w.Code)
		require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
		if method == http.MethodHead {
			require.Empty(t, w.Body.String())
		} else {
			require.Contains(t, w.Body.String(), "result_too_large")
		}
	}
	for _, e := range []error{context.DeadlineExceeded, repository.ErrNuGetResultTooLarge, errors.New("sensitive SQL and private package name")} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v3/query", nil)
		writeQueryError(c, e)
		require.Equal(t, 503, w.Code)
		require.NotContains(t, w.Body.String(), "sensitive")
	}
}

type waitingCatalog struct{ repository.NuGetCatalog }

func (waitingCatalog) Snapshot(ctx context.Context, _ func(repository.NuGetSnapshot) error) error {
	<-ctx.Done()
	return ctx.Err()
}
func TestSearchCancelsDatabaseWork(t *testing.T) {
	h := New(formats.Deps{NuGet: waitingCatalog{}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, e := h.Search(ctx, SearchScope{Repository: &domain.Repository{Name: "hosted"}, Members: []string{"hosted"}, CanRead: func(string) bool { return true }}, SearchOptions{Take: 20})
	var q *queryError
	require.ErrorAs(t, e, &q)
	require.Equal(t, "search_timeout", q.code)
}

func TestHostedSearchFailsClosedWithoutPolicy(t *testing.T) {
	h := New(formats.Deps{})
	repo := &domain.Repository{Name: "hosted", Format: "nuget", Type: domain.TypeHosted, Online: true}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v3/query", nil)
	h.serveSearch(c, repo)
	c.Writer.WriteHeaderNow()
	require.Equal(t, 503, w.Code)
	require.JSONEq(t, `{"error":"search_unavailable"}`, w.Body.String())
}
