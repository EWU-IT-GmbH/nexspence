#!/usr/bin/env python3
"""Choose a fixed-commit EWU build version and explicit publication intent."""
import json
import os
import re
from pathlib import Path

REPOSITORY = 'EWU-IT-GmbH/nexspence'


def context(env, event):
    if env.get('GITHUB_REPOSITORY') != REPOSITORY:
        raise ValueError('EWU releases only run in ' + REPOSITORY)
    sha = env['GITHUB_SHA']
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise ValueError('Expected a full commit SHA')
    ref = env['GITHUB_REF']
    kind = env['GITHUB_EVENT_NAME']
    if kind == 'push' and ref.startswith('refs/tags/'):
        version = ref.removeprefix('refs/tags/')
        if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+-ewu\.[0-9]+', version):
            raise ValueError('Release tags must look like v2.8.0-ewu.1')
        return version, True
    if kind not in ('push', 'workflow_dispatch') or not ref.startswith('refs/heads/ewu/'):
        raise ValueError('Builds must run on an ewu/* branch or an EWU release tag')
    publish = kind == 'workflow_dispatch' and event.get('inputs', {}).get('publish') in (True, 'true')
    run, attempt = env['GITHUB_RUN_ID'], env['GITHUB_RUN_ATTEMPT']
    if not run.isdigit() or not attempt.isdigit():
        raise ValueError('Invalid workflow run identifier')
    return f'v2.8.0-ewu.ci.{run}.{attempt}.{sha[:12]}', publish


if __name__ == '__main__':
    event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
    version, publish = context(os.environ, event)
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        output.write(f'version={version}\npublish={str(publish).lower()}\n')
    print(f'Version: {version}; publish: {publish}')
