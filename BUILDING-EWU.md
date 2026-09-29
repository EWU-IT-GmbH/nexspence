# EWU NuGet-search variant: build and source information

Updated: 2026-09-30. This is a modified Nexspence distribution, based on upstream
v2.5.1, commit `d5070ba188a8d96d2b3b2539d4762ad8f3b8ba62`.
It remains licensed under **AGPL-3.0-or-later**; see [LICENSE](LICENSE) and
[NOTICE](NOTICE). Maintenance: EWU-Infrastruktur-Team.

## Existing build paths retained

- Backend: `go mod download`, then `make build-backend`. `go.mod` requires
  Go 1.26.5; the NuGet acceptance tests used `golang:1.26.5`.
- Frontend: `cd frontend && npm ci && npm run build`; retain the lockfile.
- Native binary with embedded UI: `make build` builds the frontend and uses
  `-tags=embed_ui`. GoReleaser also embeds the frontend and ships LICENSE/NOTICE.
- Container: the root Dockerfile builds backend and frontend separately and
  installs the frontend at `/app/frontend/dist`. It does not use `embed_ui`.
  It accepts `VERSION`, injects `main.Version`, copies LICENSE/NOTICE, and runs
  as UID/GID 1000 with the existing entrypoint and writable directories.
- `Makefile` derives a default version from Git. A patched release must use a
  distinct version, for example `v2.5.1-ewu.1`, rather than claim to be the
  unchanged upstream v2.5.1. This example is not an allocated release tag.

The release Dockerfile now pins Go 1.26.5, Node 26 Alpine and Alpine 3.24
by image digest. See `docs/plans/nuget-search-ap-06-release.md` for the fixed-commit
build, independent rebuild comparison, source export and private publication.
Actual build/publication results must be recorded in the generated release manifest.

## Required release inputs and outputs

Build from a committed, recorded source revision, with the exact dependency
lockfiles and build scripts. The private image destination is supplied through
`NEXSPENCE_IMAGE_REPOSITORY` by the build pipeline. That variable is an agreed
pipeline input, not an environment variable already consumed by the Dockerfile.

Record the variant version, source commit, toolchain/base-image digests,
platforms, image digest, and source-archive SHA-256. Retain the upstream license,
copyright, warranty and third-party notices. The existing release workflow has
SBOM/provenance enabled; retain equivalent evidence in the private pipeline.
The image's source/revision labels must identify this variant and its actual
revision, rather than pointing exclusively to the upstream source. Labels do
not replace the user-facing source offer.

Source development repository:
`https://git.adam-crm.dev/ewu/infrastruktur/nexspence.git`.
This repository address is provenance information. It is **not** a claim that
all service users can currently download the corresponding source there.

## Source offer before network deployment

The modified service must prominently offer its complete corresponding source
to its network users, free of charge, under AGPL-3.0-or-later (LICENSE section 13).
The source must match the deployed image and include the modifications, source
needed for required linked components, build/install/run scripts, lockfiles,
license and notices. An upstream link, a patch alone, or a commit identifier
without accessible source does not suffice.

For the anonymous public feed, offer source without requiring an unrelated
private Git account. A release-specific source archive served by the deployment,
with a visible UI/source notice and discoverability for feed/API-only users,
is the proposed implementation. Exact URL, delivery mechanism and access tests
remain release work; this file does not implement the network offer.
For image recipients, also satisfy the object-code/source delivery rules of
LICENSE section 6; a network distribution can use section 6(d) with clear
adjacent source-download directions and equivalent access.

Do not include production configuration, credentials, database contents or hosted
artifacts in source archives. Provide usable configuration templates and build
instructions. Software licenses of stored packages remain separate; storing or
serving a package does not by itself relicense it as Nexspence source.

## Dependencies and scanner

No Go/npm dependencies were added for the NuGet-search patch. This does not
replace checking the license/notice obligations of the components actually
included in the final binary, frontend bundle and base image. Preserve required
third-party license texts and notices with release artifacts; a dependency
lockfile or SBOM alone is not a substitute for those texts.

The inherited NOTICE says Trivy is bundled. The current root Dockerfile does
**not** install a Trivy executable; see also `docs/scanning.md`. The appended
variant notice records this distinction without deleting upstream attribution.
If a later image adds Trivy, include its applicable license/notices and record
that addition in the release manifest.

## Validation references

NuGet acceptance and repeatable local commands:
[docs/plans/nuget-search-lokal-testen.md](docs/plans/nuget-search-lokal-testen.md).
License/build review and outstanding publication checks:
[docs/plans/nuget-search-ap-06-license.md](docs/plans/nuget-search-ap-06-license.md).
These source-tree paths are documentation references; only this build note,
LICENSE and NOTICE are explicitly copied into the runtime image at this step.
