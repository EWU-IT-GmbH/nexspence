#!/usr/bin/env python3
"""Publish a verified EWU build from GitHub Actions to GHCR/GitHub Releases."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import urllib.request

SKOPEO = 'quay.io/skopeo/stable:v1.22.0@sha256:06dd47ee861e143268f0b811cdf1f9d6509b097945de447dbcdfb668ca15364c'
REPOSITORY = 'EWU-IT-GmbH/nexspence'
IMAGE = 'ghcr.io/ewu-it-gmbh/nexspence'


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def run(*args):
    return subprocess.run(args, check=True)


def validate(release, output, env):
    if env.get('GITHUB_ACTIONS') != 'true' or env.get('EWU_PUBLISH') != 'true':
        raise RuntimeError('Publication is only allowed in an explicitly publishing GitHub Actions run')
    if env.get('GITHUB_REPOSITORY') != REPOSITORY or env.get('NEXSPENCE_IMAGE_REPOSITORY') != IMAGE:
        raise RuntimeError('Unexpected publication repository')
    if release['revision'] != env.get('GITHUB_SHA') or release['version'] != env.get('EWU_VERSION'):
        raise RuntimeError('Artifact revision/version differs from this workflow run')
    if release['runtimeManifestDigest'] != release['rebuildRuntimeManifestDigest']:
        raise RuntimeError('Reproducibility check is missing')
    if digest(output / 'image-1.oci.tar') != release['ociArchiveSHA256']:
        raise RuntimeError('Artifact checksum mismatch')
    if not env.get('GH_TOKEN'):
        raise RuntimeError('GitHub workflow token is missing')


def check_image(command, auth_path, transport, target):
    local_index = subprocess.check_output(command + ['inspect', '--raw', transport])
    expected = hashlib.sha256(local_index).hexdigest()
    check = subprocess.run(command + ['inspect', '--authfile', auth_path, '--raw', 'docker://' + target], capture_output=True)
    if check.returncode == 0:
        if hashlib.sha256(check.stdout).hexdigest() != expected:
            raise RuntimeError('Versioned image already exists with different content')
    elif not any(s in check.stderr.decode().lower() for s in ('manifest unknown', 'name unknown')):
        raise RuntimeError('Cannot verify image target absence; check GHCR permissions')
    return expected, check.returncode == 0


def verify_download(url, expected):
    # Release metadata is publicly downloadable; no authentication token.
    with urllib.request.urlopen(url, timeout=300) as response:
        if hashlib.file_digest(response, 'sha256').hexdigest() != expected:
            raise RuntimeError('Public release asset checksum mismatch')


def ensure_source_tag(version, revision):
    # Check public accessibility independently of the authenticated workflow token.
    with urllib.request.urlopen(
            f'https://api.github.com/repos/{REPOSITORY}/commits/{revision}', timeout=60) as response:
        if json.load(response).get('sha') != revision:
            raise RuntimeError('Source commit is not publicly accessible')
    endpoint = f'repos/{REPOSITORY}/git/ref/tags/{version}'
    result = subprocess.run(['gh', 'api', endpoint], capture_output=True)
    if result.returncode:
        if b'HTTP 404' not in result.stderr:
            raise RuntimeError('Cannot verify source tag')
        run('gh', 'api', '--method', 'POST', f'repos/{REPOSITORY}/git/refs',
            '-f', 'ref=refs/tags/' + version, '-f', 'sha=' + revision)
        result = subprocess.run(['gh', 'api', endpoint], capture_output=True, check=True)
    obj = json.loads(result.stdout)['object']
    # Accept both annotated and lightweight release tags, but never a moved tag.
    for _ in range(10):
        if obj['type'] != 'tag':
            break
        obj = json.loads(subprocess.check_output([
            'gh', 'api', f"repos/{REPOSITORY}/git/tags/{obj['sha']}"]))['object']
    if obj['type'] != 'commit' or obj['sha'] != revision:
        raise RuntimeError('Source tag does not point to the built commit')


def main():
    # Reject accidental local invocation before opening files or contacting services.
    if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('EWU_PUBLISH') != 'true':
        raise RuntimeError('Publication is only allowed in an explicitly publishing GitHub Actions run')
    output = Path(sys.argv[1]).resolve()
    release = json.loads((output / 'release.json').read_text())
    validate(release, output, os.environ)
    version = release['version']
    target = IMAGE + ':' + version
    source_url = f'https://github.com/{REPOSITORY}'
    source_revision_url = source_url + '/tree/' + release['revision']
    # Always use the pinned client, even if a mutable runner skopeo is installed.
    authfile = Path(os.environ.get('DOCKER_CONFIG', str(Path.home() / '.docker'))) / 'config.json'
    command = ['docker', 'run', '--rm', '-v', f'{output}:/release:ro',
               '-v', f'{authfile.resolve()}:/auth/config.json:ro', SKOPEO]
    auth_path = '/auth/config.json'
    transport = 'oci-archive:/release/image-1.oci.tar'
    expected, exists = check_image(command, auth_path, transport, target)
    ensure_source_tag(version, release['revision'])
    proof = {'image': target, 'imageIndexDigest': 'sha256:' + expected,
             'runtimeManifestDigest': release['runtimeManifestDigest'], 'sourceURL': source_url,
             'sourceRevisionURL': source_revision_url,
             'sourceTag': version, 'sourceTagURL': source_url + '/tree/' + version,
             'revision': release['revision']}
    publication = output / 'publication.json'
    publication.write_text(json.dumps(proof, indent=2) + '\n')
    assets = [output / 'release.json', publication]
    (output / 'SHA256SUMS').write_text(''.join(f'{digest(p)}  {p.name}\n' for p in assets))
    assets.append(output / 'SHA256SUMS')
    notes = output / 'release-notes.md'
    notes.write_text(f"EWU NuGet-search variant, linux/amd64.\n\nCommit: `{release['revision']}`\n\n"
                     f"Private image: `{IMAGE}@sha256:{expected}`\n\n"
                     f"Source repository: [{REPOSITORY}]({source_url}).\n\n"
                     f"Matching source revision: [{release['revision']}]({source_revision_url}).\n\n"
                     f"Source tag: `{version}` (retain permanently; do not move or delete).\n\n"
                     'See LICENSE, NOTICE and BUILDING-EWU.md in the repository.\n')
    # No --clobber, deletion or reuse: an existing release must never be overwritten.
    # A failure after creation leaves a draft for inspection; publication is the last step.
    run('gh', 'release', 'create', version, *map(str, assets), '--repo', REPOSITORY,
        '--verify-tag', '--title', 'Nexspence ' + version,
        '--notes-file', str(notes), '--draft', '--prerelease')
    if not exists:
        run(*command, 'copy', '--all', '--preserve-digests', '--authfile', auth_path,
            transport, 'docker://' + target)
    remote = subprocess.check_output(command + ['inspect', '--raw', '--authfile', auth_path, 'docker://' + target])
    if hashlib.sha256(remote).hexdigest() != expected:
        raise RuntimeError('Published OCI index digest differs from the verified artifact')
    if not any(m.get('digest') == release['runtimeManifestDigest'] for m in json.loads(remote).get('manifests', [])):
        raise RuntimeError('Published image does not reference the verified runtime manifest')
    run('gh', 'release', 'edit', version, '--repo', REPOSITORY, '--draft=false')
    for asset in assets:
        verify_download(f'https://github.com/{REPOSITORY}/releases/download/{version}/{asset.name}', digest(asset))
    summary = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary:
        with open(summary, 'a') as stream:
            stream.write(notes.read_text())
    print(json.dumps(proof, indent=2))


if __name__ == '__main__':
    main()
