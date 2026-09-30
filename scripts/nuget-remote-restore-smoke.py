#!/usr/bin/env python3
"""Opt-in live NuGet search/restore acceptance. Requires Linux, Docker and .NET 10.

Runs only isolated test repositories/PostgreSQL and an empty .NET package cache.
No production repository, registry push or deployment is used. Logs are retained
in the printed temporary directory. Outbound access to nuget.org is required.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid
from xml.sax.saxutils import escape


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--direct", action="store_true", help="Test a direct proxy instead of the group")
    args = parser.parse_args()
    for executable in ("docker", "dotnet"):
        if not shutil.which(executable):
            raise SystemExit(f"Required executable not found: {executable}")
    project = Path(__file__).resolve().parent.parent
    output = Path(tempfile.mkdtemp(prefix="nexspence-nuget-live-"))
    print(f"Acceptance logs and isolated package cache: {output}", flush=True)
    name = "nexspence-nuget-smoke-" + uuid.uuid4().hex[:12]
    command = [
        "docker", "run", "--rm", "--name", name, "--network", "host",
        "-v", "/var/run/docker.sock:/var/run/docker.sock",
        "-e", "DOCKER_API_VERSION=1.44",
        "-e", "NEXSPENCE_LIVE_NUGET_SMOKE_DIR=/smoke",
        "-e", f"NEXSPENCE_LIVE_NUGET_DIRECT={int(args.direct)}",
        "-v", f"{output}:/smoke", "-v", f"{project}:/src:ro", "-w", "/src",
        "-v", "nexspence-go-mod:/go/pkg/mod",
        "-v", "nexspence-go-build:/root/.cache/go-build",
        "golang:1.26.5", "go", "test", "-race", "-v", "-tags=integration",
        "-count=1", "./internal/formats/nuget", "-run", "TestFederatedLiveRestoreGate",
        "-timeout", "180s",
    ]
    with (output / "server.log").open("w") as server_log:
        process = subprocess.Popen(command, stdout=server_log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 90
            while not (output / "ready.json").exists():
                if process.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("Live feed did not start; inspect server.log")
                time.sleep(0.1)
            gate = json.loads((output / "ready.json").read_text())
            (output / "smoke.csproj").write_text(
                '<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup>'
                '<TargetFramework>net10.0</TargetFramework><NuGetAudit>false</NuGetAudit>'
                '</PropertyGroup><ItemGroup><PackageReference Include="NuGet.Versioning" '
                f'Version="{escape(gate["version"])}" /></ItemGroup></Project>'
            )
            (output / "NuGet.Config").write_text(
                '<configuration><packageSources><clear/>'
                f'<add key="nexspence-only" value="{escape(gate["url"])}" '
                'allowInsecureConnections="true" /></packageSources></configuration>'
            )
            env = os.environ.copy()
            env.update({
                "DOTNET_CLI_HOME": str(output / "dotnet-home"),
                "NUGET_HTTP_CACHE_PATH": str(output / "http-cache"),
                "DOTNET_SKIP_FIRST_TIME_EXPERIENCE": "1",
                "DOTNET_CLI_TELEMETRY_OPTOUT": "1",
                "DOTNET_GENERATE_ASPNET_CERTIFICATE": "false",
            })
            with (output / "restore.log").open("w") as restore_log:
                result = subprocess.run([
                    "dotnet", "restore", str(output / "smoke.csproj"),
                    "--configfile", str(output / "NuGet.Config"),
                    "--packages", str(output / "packages"), "--force",
                    "--no-http-cache", "--verbosity", "normal",
                ], env=env, stdout=restore_log, stderr=subprocess.STDOUT, timeout=90)
            done = output / "done.tmp"
            done.write_text(json.dumps({"exitCode": result.returncode}))
            done.replace(output / "done.json")
            server_exit = process.wait(timeout=30)
            print((output / "server.log").read_text())
            if result.returncode or server_exit:
                raise RuntimeError("Live acceptance failed; inspect server.log and restore.log")
            print(f"PASS: clean restore of NuGet.Versioning {gate['version']} through {"direct proxy" if args.direct else "group"}")
        finally:
            if process.poll() is None:
                subprocess.run(["docker", "stop", "-t", "1", name],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               timeout=15, check=False)
                process.wait(timeout=15)


if __name__ == "__main__":
    main()
