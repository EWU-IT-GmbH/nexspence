package nuget_test

import (
	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/formats/nuget"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestNuGetGroupRejectsQueryMutationBeforeFallback(t *testing.T) {
	h := nuget.New(formats.Deps{})
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/v3/query", nil)
		c.Params = gin.Params{{Key: "path", Value: "/v3/query"}}
		require.True(t, h.ServeGroup(c, &domain.Repository{}, func([]string) { t.Fatal("unexpected member call") }))
		c.Writer.WriteHeaderNow()
		require.Equal(t, 405, w.Code)
		require.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
	}
}
