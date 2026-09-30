package nuget_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/nexspence-oss/nexspence/internal/formats/repoproxy"
)

// TestMain installs an unguarded upstream HTTP client for this packages proxy
// tests. Production UpstreamClient is SSRF-guarded and would block the loopback
// httptest servers these tests use as fake upstreams.
var cleanupIntegration = func() {}

func TestMain(m *testing.M) {
	repoproxy.UpstreamClient = &http.Client{}
	code := m.Run()
	cleanupIntegration()
	os.Exit(code)
}
