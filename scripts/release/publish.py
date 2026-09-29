#!/usr/bin/env python3
"""Publish verified OCI/source artifacts to explicitly configured image/source targets.
Targets: NEXSPENCE_IMAGE_REPOSITORY and NEXSPENCE_SOURCE_REPOSITORY_URL.
Docker login must already be configured. Raw HTTP auth uses SOURCE_USERNAME /
SOURCE_PASSWORD (NEXSPENCE_ prefix) or the matching Docker auth host entry.
"""
import base64
import hashlib
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

SKOPEO = 'quay.io/skopeo/stable:v1.22.0@sha256:06dd47ee861e143268f0b811cdf1f9d6509b097945de447dbcdfb668ca15364c'


def digest(path):
    with path.open('rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def main():
    output = Path(sys.argv[1]).resolve()
    release = json.loads((output / 'release.json').read_text())
    host = os.environ.get('ARTIFACTS_URL', '').removeprefix('https://').rstrip('/')
    image_repo = os.environ.get('NEXSPENCE_IMAGE_REPOSITORY') or '/'.join([
        host, os.environ['DOCKER_PRIVATE_REGISTRY_NAMESPACE'], 'nexspence'])
    image_repo = image_repo.rstrip('/')
    raw = os.environ['NEXSPENCE_SOURCE_REPOSITORY_URL'].rstrip('/')
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme != 'https' or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise RuntimeError('Source destination must be HTTPS without credentials/query/fragment')
    if '://' in image_repo or '@' in image_repo or ':' in image_repo.split('/')[-1]:
        raise RuntimeError('Image repository must not include a scheme, tag or digest')
    if release['runtimeManifestDigest'] != release['rebuildRuntimeManifestDigest']:
        raise RuntimeError('Reproducibility check is missing')
    source = output / release['sourceArchive']
    image = output / 'image-1.oci.tar'
    if digest(source) != release['sourceSHA256'] or digest(image) != release['ociArchiveSHA256']:
        raise RuntimeError('Artifact checksum mismatch')
    authfile = Path(os.environ.get('DOCKER_CONFIG', str(Path.home() / '.docker'))) / 'config.json'
    auths = json.loads(authfile.read_text()).get('auths', {})
    username = os.environ.get('ARTIFACTS_USER') or os.environ.get('NEXSPENCE_SOURCE_USERNAME')
    password = os.environ.get('ARTIFACTS_PASSWORD') or os.environ.get('NEXSPENCE_SOURCE_PASSWORD')
    if username and password:
        auth = base64.b64encode((username + ':' + password).encode()).decode()
    else:
        auth = auths.get(parsed.netloc, {}).get('auth')
    if not auth:
        raise RuntimeError('No raw repository credentials configured')
    # Never redirect a credential-bearing upload to a different endpoint.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None
    opener = urllib.request.build_opener(NoRedirect())
    def request(url, method='GET', data=None, authenticate=True):
        r = urllib.request.Request(url, method=method, data=data)
        if authenticate:
            r.add_header('Authorization', 'Basic ' + auth)
        if data is not None:
            r.add_header('Content-Type', 'application/octet-stream')
        return opener.open(r, timeout=300)
    public_source = os.environ.get('NEXSPENCE_SOURCE_PUBLIC', 'false').lower() == 'true'
    def upload(path):
        url = raw + '/' + release['version'] + '/' + path.name
        try:
            with request(url, 'HEAD', authenticate=False):
                if not public_source:
                    raise RuntimeError('Requested private source destination is anonymously accessible')
        except urllib.error.HTTPError as e:
            if e.code not in (401, 403, 404):
                raise
        try:
            with request(url) as r:
                existing = hashlib.file_digest(r, 'sha256').hexdigest()
            if existing != digest(path):
                raise RuntimeError('Refusing to overwrite a different published artifact: ' + path.name)
        except urllib.error.HTTPError as e:
            if e.code != 404:
                raise
            with path.open('rb') as stream:
                req = urllib.request.Request(url, data=stream, method='PUT', headers={
                    'Authorization': 'Basic ' + auth, 'Content-Length': str(path.stat().st_size),
                    'Content-Type': 'application/octet-stream'})
                with opener.open(req, timeout=300):
                    pass
        with request(url) as r:
            if hashlib.file_digest(r, 'sha256').hexdigest() != digest(path):
                raise RuntimeError('Remote source checksum mismatch')
        if public_source:
            with request(url, authenticate=False) as r:
                if hashlib.file_digest(r, 'sha256').hexdigest() != digest(path):
                    raise RuntimeError('Anonymous source download checksum mismatch')
        return url
    # For private publication, reject an anonymously accessible repository.
    if not public_source:
        try:
            with request(raw + '/', 'HEAD', authenticate=False):
                raise RuntimeError('Raw repository is not private; refusing publication')
        except urllib.error.HTTPError as e:
            if e.code not in (401, 403):
                raise RuntimeError('Cannot establish private raw repository access policy') from e
    source_url = upload(source)
    upload(output / 'SHA256SUMS')
    upload(output / 'release.json')
    target = image_repo + ':' + release['version']
    if shutil.which('skopeo'):
        command = ['skopeo']
        auth_path = str(authfile.resolve())
        image_transport = 'oci-archive:' + str(image)
    else:
        command = ['docker', 'run', '--rm', '-v', f'{output}:/release:ro',
                   '-v', f'{authfile.resolve()}:/auth/config.json:ro', SKOPEO]
        auth_path = '/auth/config.json'
        image_transport = 'oci-archive:/release/image-1.oci.tar'
    # Do not replace a different versioned image. A repeated identical copy is safe.
    check = subprocess.run(command + ['inspect', '--authfile', auth_path,
                         '--raw', 'docker://' + target], capture_output=True)
    if check.returncode == 0:
        remote = json.loads(check.stdout)
        manifests = remote.get('manifests', [])
        if not any(m.get('digest') == release['runtimeManifestDigest'] for m in manifests):
            raise RuntimeError('Versioned image already exists with different content')
    elif not any(s in check.stderr.decode().lower() for s in ('manifest unknown', 'name unknown')):
        raise RuntimeError('Cannot verify image target absence; check registry login and permissions')
    subprocess.run(command + ['copy', '--all', '--preserve-digests', '--authfile', auth_path,
                   image_transport, 'docker://' + target], check=True)
    raw_index = subprocess.check_output(command + ['inspect', '--raw', '--authfile',
                                    auth_path, 'docker://' + target])
    index = json.loads(raw_index)
    if not any(m.get('digest') == release['runtimeManifestDigest'] for m in index.get('manifests', [])):
        raise RuntimeError('Published image does not reference the verified runtime manifest')
    proof = {'image': target, 'imageIndexDigest': 'sha256:' + hashlib.sha256(raw_index).hexdigest(),
             'runtimeManifestDigest': release['runtimeManifestDigest'], 'sourceURL': source_url,
             'sourceSHA256': release['sourceSHA256'], 'revision': release['revision']}
    (output / 'publication.json').write_text(json.dumps(proof, indent=2) + '\n')
    upload(output / 'publication.json')
    print(json.dumps(proof, indent=2))


if __name__ == '__main__':
    main()
