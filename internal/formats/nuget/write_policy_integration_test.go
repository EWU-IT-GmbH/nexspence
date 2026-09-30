//go:build integration

package nuget_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/stretchr/testify/require"
)

// Verify that the shared 2.9 write policy protects both NuGet package bytes
// and searchable metadata when a client attempts to replace a version.
func TestHostedWritePolicyPreservesPackageAndSearch(t *testing.T) {
	for _, policy := range []string{"allow", "allow_once", "deny"} {
		t.Run(policy, func(t *testing.T) {
			f := hosted(t)
			original := buildNupkg(t, "Example.Lib", "1.0.0")
			replacement := buildNupkg(t, "example.lib", "1.0.0")
			require.Equal(t, http.StatusCreated, pushNupkg(f.router, f.repo.Name, "example.lib.1.0.0.nupkg", string(original)))
			f.repo.FormatConfig = map[string]any{domain.WritePolicyKey: policy}
			require.NoError(t, f.deps.Repos.Update(context.Background(), f.repo))
			code := pushNupkg(f.router, f.repo.Name, "example.lib.1.0.0.nupkg", string(replacement))
			expected := original
			if policy == "allow" {
				require.Equal(t, http.StatusCreated, code)
				expected = replacement
			} else {
				require.Equal(t, http.StatusBadRequest, code)
			}
			got := f.get(t, "/v3/flatcontainer/example.lib/1.0.0/example.lib.1.0.0.nupkg")
			require.Equal(t, http.StatusOK, got.Code)
			require.Equal(t, expected, got.Body.Bytes())
			result := f.search(t, "?q=example.lib")
			require.Len(t, result.Data, 1)
			require.Equal(t, "1.0.0", result.Data[0].Version)
			fresh := pushNupkg(f.router, f.repo.Name, "example.lib.2.0.0.nupkg", string(buildNupkg(t, "Example.Lib", "2.0.0")))
			if policy == "deny" {
				require.Equal(t, http.StatusBadRequest, fresh)
			} else {
				require.Equal(t, http.StatusCreated, fresh)
			}
		})
	}
}
