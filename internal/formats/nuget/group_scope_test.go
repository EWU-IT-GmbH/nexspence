package nuget

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/formats"
	"github.com/nexspence-oss/nexspence/internal/logger"
	"github.com/nexspence-oss/nexspence/internal/repository"
	"github.com/nexspence-oss/nexspence/internal/service"
	"github.com/nexspence-oss/nexspence/internal/testutil"
	"github.com/stretchr/testify/require"
)

type unavailableMemberRepo struct{ repository.RepositoryRepo }

func (r unavailableMemberRepo) Get(ctx context.Context, name string) (*domain.Repository, error) {
	if name == "second" {
		return nil, errors.New("private database failure")
	}
	return r.RepositoryRepo.Get(ctx, name)
}

type unavailableRuleRepo struct{ repository.RoutingRuleRepo }

func (unavailableRuleRepo) Get(context.Context, string) (*domain.RoutingRule, error) {
	return nil, errors.New("private rule failure")
}

type unavailableCatalog struct{ repository.NuGetCatalog }

func (unavailableCatalog) Snapshot(context.Context, func(repository.NuGetSnapshot) error) error {
	return errors.New("private catalog failure")
}

func TestGroupFailuresDoNotReturnPartialResults(t *testing.T) {
	for _, failure := range []string{"member", "rule", "catalog", "policy"} {
		t.Run(failure, func(t *testing.T) {
			group := &domain.Repository{Name: "all", Format: "nuget", Type: domain.TypeGroup, Online: true, FormatConfig: map[string]any{"member_names": []string{"first", "second"}}}
			first := &domain.Repository{Name: "first", Format: "nuget", Type: domain.TypeHosted, Online: true}
			second := &domain.Repository{Name: "second", Format: "nuget", Type: domain.TypeHosted, Online: true}
			d := formats.Deps{Repos: testutil.NewRepoRepo(group, first, second), NuGet: &testutil.NuGetCatalog{Components: testutil.NewComponentRepo(), Assets: testutil.NewAssetRepo()}}
			d.RBAC = service.NewRBACService(nil, d.Repos, logger.New("error", "json"), true)
			switch failure {
			case "catalog":
				d.NuGet = unavailableCatalog{}
			case "member":
				d.Repos = unavailableMemberRepo{d.Repos}
			case "rule":
				id := "missing"
				group.RoutingRuleID = &id
				d.RoutingRules = unavailableRuleRepo{}
			case "policy":
				d.RBAC = nil
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/v3/query", nil)
			c.Set("userID", "admin")
			c.Set("roles", []string{"nx-admin"})
			New(d).serveSearch(c, group)
			c.Writer.WriteHeaderNow()
			require.Equal(t, 503, w.Code)
			require.JSONEq(t, `{"error":"search_unavailable"}`, w.Body.String())
		})
	}
}
