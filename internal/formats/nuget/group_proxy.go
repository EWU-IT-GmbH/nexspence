package nuget

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/nexspence-oss/nexspence/internal/domain"
	"github.com/nexspence-oss/nexspence/internal/nugetmeta"
)

// A group's locally advertised paths differ from the upstream's resource paths
// (for example /v3/flatcontainer versus nuget.org's /v3-flatcontainer). Discover
// the matching resource only for an explicit remote read, never for local search.
func groupProxyPath(ctx context.Context, repo *domain.Repository, p string) (string, error) {
	prefix := "/v3/flatcontainer/"
	kinds := []string{"PackageBaseAddress/3.0.0"}
	if strings.HasPrefix(p, registrationRoot(true)) {
		prefix = registrationRoot(true)
		kinds = []string{"RegistrationsBaseUrl/3.6.0"}
	} else if strings.HasPrefix(p, registrationRoot(false)) {
		prefix = registrationRoot(false)
		kinds = []string{"RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.0.0", "RegistrationsBaseUrl"}
	}
	if !strings.HasPrefix(p, prefix) {
		return "", unavailable("search_unavailable")
	}
	resource, err := discoverNuGetResource(ctx, repo, kinds)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(resource)
	if err != nil || u.RawQuery != "" {
		return "", unavailable("invalid_upstream_resource")
	}
	return strings.TrimRight(resource, "/") + "/" + strings.TrimPrefix(p, prefix), nil
}

// Keep remote registration links on the caller-facing group and registration
// hive, including leaves without an inline catalogEntry object.
func rewriteGroupRegistration(body []byte, localBase string, sem2 bool, remoteRoot string) []byte {
	body = RewriteRegistration(body, localBase)
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, value := range node {
				if raw, ok := value.(string); ok && (key == "@id" || key == "registration" || key == "packageContent") {
					if remoteRoot != "" && strings.HasPrefix(raw, remoteRoot) && key != "packageContent" {
						node[key] = localBase + registrationRoot(sem2) + strings.TrimPrefix(raw, remoteRoot)
						continue
					}
					u, err := url.Parse(raw)
					if err == nil && u.IsAbs() {
						parts := strings.Split(strings.Trim(u.Path, "/"), "/")
						if key == "packageContent" && len(parts) >= 3 {
							tail := parts[len(parts)-3:]
							v, err := nugetmeta.ParseVersion(tail[1])
							if err == nil && nugetmeta.ValidID(tail[0]) && strings.EqualFold(tail[2], tail[0]+"."+tail[1]+".nupkg") {
								id := strings.ToLower(tail[0])
								node[key] = localBase + packagePaths(id, v.Key(), sem2)[1]
								continue
							}
						}
						for i, part := range parts {
							if strings.HasPrefix(part, "registration") && i+1 < len(parts) {
								node[key] = localBase + registrationRoot(sem2) + strings.Join(parts[i+1:], "/")
								break
							}
							if strings.Contains(part, "flatcontainer") && i+1 < len(parts) {
								node[key] = localBase + "/v3/flatcontainer/" + strings.Join(parts[i+1:], "/")
								break
							}
						}
					}
				} else {
					walk(value)
				}
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return b
}
