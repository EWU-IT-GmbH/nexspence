#!/usr/bin/env python3
"""Isolated NuGet client acceptance (Linux Docker, Python 3, .NET SDK 10).

Uses production Auth/RBAC/NuGet handlers with PostgreSQL, test blob storage and
synthetic packages. No production credentials or feeds. --rider keeps the feed
open for five minutes after automated checks; Enter finishes it early.
"""
import argparse
import base64
import json
import gzip
import os
from pathlib import Path
import select
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from xml.sax.saxutils import escape


def private_write(path, text):
    path.write_text(text)
    path.chmod(0o600)


def config(path, url, user, token):
    private_write(path, '<configuration><packageSources><clear/>'
                  f'<add key="local" value="{escape(url)}" allowInsecureConnections="true"/>'
                  '</packageSources><packageSourceCredentials><local>'
                  f'<add key="Username" value="{user}"/>'
                  f'<add key="ClearTextPassword" value="{token}"/>'
                  '<add key="ValidAuthenticationTypes" value="basic"/>'
                  '</local></packageSourceCredentials></configuration>')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rider', action='store_true')
    parser.add_argument('--rider-minutes', type=int, default=5, choices=range(1, 61),
                        metavar='1..60', help='Rider session duration (default: 5 minutes)')
    args = parser.parse_args()
    hold_seconds = args.rider_minutes * 60 if args.rider else 0
    for executable in ('docker', 'dotnet'):
        if not shutil.which(executable):
            raise SystemExit(f'Required executable not found: {executable}')
    project = Path(__file__).resolve().parent.parent
    output = Path(tempfile.mkdtemp(prefix='nexspence-nuget-local-'))
    print(f'Test files, credentials and logs: {output}', flush=True)
    name = 'nexspence-client-' + uuid.uuid4().hex[:12]
    command = [
        'docker', 'run', '--rm', '--name', name, '--network', 'host',
        '-v', '/var/run/docker.sock:/var/run/docker.sock', '-e', 'DOCKER_API_VERSION=1.44',
        '-e', 'NEXSPENCE_LOCAL_NUGET_DIR=/smoke',
        '-e', f'NEXSPENCE_LOCAL_NUGET_WAIT_SECONDS={hold_seconds + 180}',
        '-e', f'PGTEST_TTL_SECONDS={max(600, hold_seconds + 360)}',
        '-v', f'{output}:/smoke',
        '-v', f'{project}:/src:ro', '-w', '/src',
        '-v', 'nexspence-go-mod:/go/pkg/mod', '-v', 'nexspence-go-build:/root/.cache/go-build',
        'golang:1.26.5', 'go', 'test', '-race', '-v', '-tags=integration', '-count=1',
        './internal/formats/nuget', '-run', '^TestLocalNuGetClientGate$', '-timeout', f'{hold_seconds + 300}s',
    ]
    env = os.environ.copy()
    # Never inherit the caller's feed credential override for our source name.
    env.pop('NuGetPackageSourceCredentials_local', None)
    env.update(DOTNET_CLI_HOME=str(output / 'dotnet-home'),
               NUGET_HTTP_CACHE_PATH=str(output / 'http-cache'),
               DOTNET_SKIP_FIRST_TIME_EXPERIENCE='1', DOTNET_CLI_TELEMETRY_OPTOUT='1',
               DOTNET_GENERATE_ASPNET_CERTIFICATE='false')

    def run(label, *arguments):
        with (output / f'{label}.log').open('w') as log:
            result = subprocess.run(['dotnet', *arguments], cwd=output, env=env,
                                    stdout=log, stderr=subprocess.STDOUT, timeout=90)
        if result.returncode:
            raise RuntimeError(f'{label} failed; inspect {output / (label + ".log")}')

    with (output / 'server.log').open('w') as server_log:
        process = subprocess.Popen(command, stdout=server_log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 120
            while not (output / 'ready.json').exists():
                if process.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('Feed did not start; inspect server.log')
                time.sleep(0.1)
            gate = json.loads((output / 'ready.json').read_text())
            base = gate['url']
            config(output / 'NuGet.Config', base + '/repository/nuget/index.json', 'reader', gate['reader'])
            config(output / 'push.config', base + '/repository/hosted/index.json', 'ci', gate['writer'])
            private_write(output / 'credentials.txt', f"Reader: reader\nAPI token: {gate['reader']}\n")

            def get(path, token=None, expected=200):
                request = urllib.request.Request(base + path)
                if token:
                    encoded = base64.b64encode(('reader:' + token).encode()).decode()
                    request.add_header('Authorization', 'Basic ' + encoded)
                try:
                    response = urllib.request.urlopen(request, timeout=10)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    body = response.read()
                    if response.status != expected:
                        raise RuntimeError(f'{path}: expected {expected}, got {response.status}')
                    return json.loads(body) if expected == 200 else None

            index = get('/repository/nuget/index.json', gate['reader'])
            query = next(r['@id'] for r in index['resources'] if r['@type'].startswith('SearchQueryService'))
            assert query == base + '/repository/nuget/v3/query', query
            result = get('/repository/nuget/v3/query?q=ewu.trace&semVerLevel=2.0.0', gate['reader'])
            trace = next(p for p in result['data'] if p['id'] == 'ewu.trace')
            assert trace['version'] == '1.10.0', trace
            assert len(trace['versions']) == 2, trace
            leaf = trace['versions'][0]['@id']
            # urllib does not request compression; handle registration gzip explicitly.
            req = urllib.request.Request(leaf)
            req.add_header('Authorization', 'Basic ' + base64.b64encode(('reader:' + gate['reader']).encode()).decode())
            with urllib.request.urlopen(req, timeout=10) as response:
                body = response.read()
                if response.headers.get('Content-Encoding') == 'gzip':
                    body = gzip.decompress(body)
                assert json.loads(body)['packageContent'].startswith(base + '/repository/nuget/')
            public = get('/repository/nuget-public/v3/query')
            assert [p['id'] for p in public['data']] == ['ewu.public'], public
            assert 'Private' not in json.dumps(public) and '1.10.0' not in json.dumps(public)
            get('/repository/nuget/v3/query', expected=401)
            get('/repository/hosted/v3/query', gate['reader'], expected=403)

            # Exercise actual NuGet.Protocol resource discovery, not just a JSON lookup.
            search_client = output / 'search-client'
            shutil.copytree(project / 'scripts/nuget-client-search', search_client,
                            ignore=shutil.ignore_patterns('bin', 'obj'))
            run('search-client', 'run', '--project', str(search_client / 'check.csproj'),
                '--', str(output))

            pack = output / 'pack'
            pack.mkdir()
            (pack / 'ci.csproj').write_text('<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup>'
                '<TargetFramework>net10.0</TargetFramework><PackageId>ewu.ci</PackageId>'
                '<Version>1.0.0</Version><NuGetAudit>false</NuGetAudit></PropertyGroup></Project>')
            (pack / 'Trace.cs').write_text('public class Trace { public static string Version => "1.0.0"; }')
            run('pack', 'pack', str(pack / 'ci.csproj'), '-o', str(output / 'nupkgs'),
                '--configfile', str(output / 'NuGet.Config'))
            run('push', 'nuget', 'push', str(output / 'nupkgs/ewu.ci.1.0.0.nupkg'),
                '--source', 'local', '--configfile', str(output / 'push.config'),
                '--api-key', gate['writer'], '--no-symbols', '--allow-insecure-connections')
            result = get('/repository/nuget/v3/query?q=ewu.ci', gate['reader'])
            assert result['totalHits'] == 1 and result['data'][0]['version'] == '1.0.0', result
            # Separate directory avoids compiling the package producer or its obj files.
            consumer = output / 'consumer'
            consumer.mkdir()
            (consumer / 'acceptance.csproj').write_text('<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup>'
                '<TargetFramework>net10.0</TargetFramework><NuGetAudit>false</NuGetAudit>'
                f'<RestorePackagesPath>{escape(str(output / "packages"))}</RestorePackagesPath>'
                '</PropertyGroup><ItemGroup><PackageReference Include="ewu.ci" Version="1.0.0"/>'
                '<PackageReference Include="ewu.trace" Version="1.10.0"/>'
                '</ItemGroup></Project>')
            run('restore', 'restore', str(consumer / 'acceptance.csproj'), '--configfile',
                str(output / 'NuGet.Config'), '--packages', str(output / 'packages'),
                '--force', '--no-http-cache')
            assert (output / 'packages/ewu.ci/1.0.0/ewu.ci.1.0.0.nupkg').is_file()
            print('Client checks passed: authenticated search, public isolation, API-token push and clean .NET restore.', flush=True)
            if args.rider:
                print(f'Open {consumer / "acceptance.csproj"} in Rider. Feed: {base}/repository/nuget/index.json', flush=True)
                print('Credentials are in NuGet.Config / credentials.txt. Search ewu.trace; expect 1.9.0, 1.10.0 and (with prereleases) 2.0.0-beta.2.', flush=True)
                print(f'Feed remains available for {args.rider_minutes} minutes. Press Enter to stop; this does NOT mark Rider acceptance as passed.', flush=True)
                if sys.stdin.isatty():
                    select.select([sys.stdin], [], [], hold_seconds)
                else:
                    time.sleep(hold_seconds)
            (output / 'done.tmp').write_text('ok')
            (output / 'done.tmp').replace(output / 'done.json')
            if process.wait(timeout=30):
                raise RuntimeError('Server-side acceptance failed; inspect server.log')
            print('PASS: automated local client acceptance (Rider acceptance is separate).', flush=True)
        finally:
            if process.poll() is None:
                # Signal graceful test teardown (also purges the PostgreSQL container).
                (output / 'done.tmp').write_text('failed')
                (output / 'done.tmp').replace(output / 'done.json')
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    subprocess.run(['docker', 'stop', '-t', '1', name],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   timeout=15, check=False)
                    process.wait(timeout=15)


if __name__ == '__main__':
    main()
