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

## Reproducible container build

The Dockerfile pins Go 1.26.5, Node 26 Alpine and Alpine 3.24 by image digest.
The release script builds the exact committed revision twice without cache and
requires equal linux/amd64 runtime manifest digests. SOURCE_DATE_EPOCH comes from
the commit timestamp. SBOM and provenance attestations are retained; their
run-specific metadata can change the enclosing OCI index digest on a rebuild.

Requirements: Python 3.12+, Git, Docker Buildx with OCI export support, and access
to the pinned base images and locked Go/npm dependencies. The GitHub workflow
selects Buildx 0.37.1 and pins BuildKit 0.33.0 by image digest.

```bash
python3 scripts/release/build.py --revision HEAD --version v2.5.1-ewu.1 \
  --output /absolute/new/release-directory
```

The output directory must not already exist. The script uses committed files;
uncommitted changes are not part of the build. Outputs include two OCI archives,
release.json, checksums and build logs. The temporary checkout tar is only a build
input; no separate source archive is generated or published.

## GitHub Actions and publication

[EWU build and release](.github/workflows/ewu-release.yml) runs only in
`EWU-IT-GmbH/nexspence`:

- Push an `ewu/*` branch: build and verify, without publishing.
- Push an EWU release tag such as `v2.5.1-ewu.1`: build and publish that version.
- Run manually on an `ewu/*` branch: build only by default; explicitly select
  `publish` for a uniquely versioned prerelease. Its version contains run ID,
  attempt and commit short SHA, so a manual retry gets a fresh version.

Enable Actions in the fork if prompted. Manual dispatch requires the workflow
on the repository's default branch; `ewu/v2.5.1` is the intended EWU branch.
No default-branch or organization settings are changed by the workflow.
The automatic GITHUB_TOKEN receives contents:write and packages:write. No
personal access token, GitLab CI variables or Nexspence API credentials are needed.
Organization policies must permit Actions and creation of the GHCR package.

The private image target is `ghcr.io/ewu-it-gmbh/nexspence:<version>`.
The workflow verifies anonymous access to the matching source commit and ensures
that the release tag points to it, creating a missing tag at that exact commit.
Keep published tags and their commits permanently; configure repository rules to
prevent tag updates/deletion. The scripts reject conflicts but do not install
those repository rules themselves.

Publication first creates a draft GitHub prerelease with release.json,
publication.json and SHA256SUMS. It then copies the OCI image with its attestations,
verifies the remote digest, publishes the release and verifies public metadata
downloads. The release notes link to the repository and exact source revision.
The private OCI archives are not uploaded as public release or Actions artifacts.
Only JSON metadata, checksums and build logs are retained in Actions for 30 days.

Existing releases/images are never overwritten. A failed publication can leave
a tag, draft release or uploaded image for inspection. Do not blindly rerun a
partially published tagged build: its attestations can differ. Recover the
original artifacts or use a new release version. Publication outside an explicit
publishing GitHub Actions run is rejected.

No deployment or ArgoCD sync is included. For the later Kubernetes rollout use
the recorded image digest and a separate read-only GHCR credential. Inherited
upstream release/tag/website deployment workflows have been removed from this fork.
The first real EWU Actions run is still pending; local checks do not establish
successful publication or deployment.

## Required release inputs and outputs

Build from a committed, recorded source revision, with the exact dependency
lockfiles and build scripts. The private image destination is supplied through
`NEXSPENCE_IMAGE_REPOSITORY` by the build pipeline. That variable is an agreed
pipeline input, not an environment variable already consumed by the Dockerfile.

Record the variant version, source commit, toolchain/base-image digests,
platforms, image digest, and permanent source tag/commit. Retain the upstream license,
copyright, warranty and third-party notices. The existing release workflow has
SBOM/provenance enabled; retain equivalent evidence in the private pipeline.
The image's source/revision labels must identify this variant and its actual
revision, rather than pointing exclusively to the upstream source. Labels do
not replace the user-facing source offer.

Public source development repository:
`https://github.com/EWU-IT-GmbH/nexspence`.
The visible source link points to this public repository. Image/release metadata
identifies the exact matching commit and permanent Git tag. No separate source
archive is generated. Keep published tags and their commits publicly accessible.
Rebuilding requires access to the pinned base images and locked Go/npm dependencies.

## Source offer before network deployment

The modified service must prominently offer its complete corresponding source
to its network users, free of charge, under AGPL-3.0-or-later (LICENSE section 13).
The source must match the deployed image and include the modifications, source
needed for required linked components, build/install/run scripts, lockfiles,
license and notices. An upstream link, a patch alone, or a commit identifier
without accessible source does not suffice.

The planned visible source notice points to the public repository at
`https://github.com/EWU-IT-GmbH/nexspence`. Release metadata identifies the exact
commit and permanent Git tag corresponding to the image. UI/source notice
and discoverability for feed/API-only users remain separate deployment work;
this build note does not implement that user-facing offer.
For image recipients, also satisfy the object-code/source delivery rules of
LICENSE section 6; a network distribution can use section 6(d) with clear
adjacent source-download directions and equivalent access.

Do not include production configuration, credentials, database contents or hosted
artifacts in the public source repository. Provide usable configuration templates and build
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

## Validation

Run isolated release-tooling tests without Docker, tokens or network access:

```bash
python3 -m unittest discover -s scripts/release -p 'test_*.py' -v
```

NuGet test coverage is in `internal/formats/nuget`, `internal/nugetmeta` and
`internal/service`. Client acceptance helpers are in
`scripts/nuget-client-search`, `scripts/nuget-local-acceptance.py` and
`scripts/nuget-remote-restore-smoke.py`. These integration helpers require a test
instance; inspect their usage before running them. Full upstream test-suite
success is not claimed by the release-tooling checks.
