#!/usr/bin/env python3
"""Build a committed revision twice; export OCI images and fixed source revision metadata.
Requires Python 3, Git, Docker Buildx (OCI exporter), network for the first build.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def runtime_manifest(path):
    with tarfile.open(path) as archive:
        def blob(digest):
            return json.load(archive.extractfile('blobs/' + digest.replace(':', '/')))
        def walk(index):
            for descriptor in index['manifests']:
                platform = descriptor.get('platform', {})
                if platform.get('os') == 'linux' and platform.get('architecture') == 'amd64':
                    return descriptor['digest']
                if descriptor['mediaType'].endswith(('image.index.v1+json', 'manifest.list.v2+json')):
                    found = walk(blob(descriptor['digest']))
                    if found:
                        return found
        digest = walk(json.load(archive.extractfile('index.json')))
        if not digest:
            raise RuntimeError('No linux/amd64 runtime manifest in OCI output')
        return digest


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--revision', default='HEAD')
    p.add_argument('--version', required=True)
    p.add_argument('--output', required=True)
    args = p.parse_args()
    if not re.fullmatch(r'v[0-9][A-Za-z0-9_.-]*', args.version):
        p.error('version must be a distinct release identifier starting with v and a digit')
    revision = git('rev-parse', args.revision + '^{commit}')
    epoch = git('show', '-s', '--format=%ct', revision)
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    context = output / 'context'
    context.mkdir()
    # Only committed content enters the temporary build context.
    tree = output / 'checkout.tar'
    run('git', 'archive', '--format=tar', '-o', str(tree), revision)
    with tarfile.open(tree) as tar:
        tar.extractall(context, filter='data')
    common = ['docker', 'buildx', 'build', '--platform', 'linux/amd64',
              '--build-arg', 'VERSION=' + args.version, '--build-arg', 'REVISION=' + revision,
              '--build-arg', 'SOURCE_DATE_EPOCH=' + epoch]
    if os.environ.get('BUILDX_BUILDER'):
        common += ['--builder', os.environ['BUILDX_BUILDER']]
    digests = []
    for number in (1, 2):
        image = output / f'image-{number}.oci.tar'
        with (output / f'build-{number}.log').open('w') as log:
            run(*common, '--target', 'runtime', '--no-cache', '--provenance=mode=max', '--sbom=true',
                '--output', f'type=oci,dest={image},rewrite-timestamp=true',
                '--metadata-file', str(output / f'build-{number}.json'), str(context),
                stdout=log, stderr=subprocess.STDOUT)
        digests.append(runtime_manifest(image))
        print(f'Build {number} runtime manifest: {digests[-1]}', flush=True)
    if digests[0] != digests[1]:
        raise RuntimeError('Independent runtime image manifests differ')
    metadata = {'dockerBuildx': subprocess.check_output(['docker', 'buildx', 'version'], text=True).strip(),
                'version': args.version, 'revision': revision, 'sourceDateEpoch': int(epoch),
                'platform': 'linux/amd64', 'runtimeManifestDigest': digests[0],
                'rebuildRuntimeManifestDigest': digests[1],
                'note': 'SBOM/provenance attestations have run-specific metadata; runtime manifests are compared.'}
    metadata['sourceURL'] = 'https://github.com/EWU-IT-GmbH/nexspence'
    metadata['sourceRevisionURL'] = metadata['sourceURL'] + '/tree/' + revision
    metadata['sourceTag'] = args.version
    metadata['ociArchiveSHA256'] = sha(output / 'image-1.oci.tar')
    (output / 'release.json').write_text(json.dumps(metadata, indent=2) + '\n')
    # The private OCI archive is not a public release asset.
    (output / 'SHA256SUMS').write_text(f"{sha(output / 'release.json')}  release.json\n")
    print(json.dumps(metadata, indent=2), flush=True)


if __name__ == '__main__':
    main()
