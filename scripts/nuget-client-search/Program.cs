using System;
using System.Linq;
using System.Threading;
using NuGet.Common;
using NuGet.Configuration;
using NuGet.Protocol;
using NuGet.Protocol.Core.Types;
var settings = Settings.LoadSpecificSettings(args[0], "NuGet.Config");
var source = new PackageSourceProvider(settings).LoadPackageSources().Single(s => s.Name == "local");
var repo = new SourceRepository(source, Repository.Provider.GetCoreV3());
var search = await repo.GetResourceAsync<PackageSearchResource>();
foreach (var prerelease in new[] { false, true })
{
    var results = (await search.SearchAsync("ewu.trace", new SearchFilter(prerelease), 0, 20,
        NullLogger.Instance, CancellationToken.None)).ToList();
    var trace = results.Single(p => p.Identity.Id == "ewu.trace");
    var expected = prerelease ? "2.0.0-beta.2" : "1.10.0";
    if (trace.Identity.Version.ToNormalizedString() != expected)
        throw new Exception("Unexpected latest version: " + trace.Identity);
    var versions = (await trace.GetVersionsAsync()).Select(v => v.Version.ToNormalizedString()).ToList();
    if (versions.Count != (prerelease ? 3 : 2) || !versions.Contains("1.9.0") || !versions.Contains("1.10.0"))
        throw new Exception("Incomplete version list");
    if (!results.Any(p => p.Identity.Id == "ewu.trace.tools"))
        throw new Exception("ewu.trace.tools missing");
    Console.WriteLine($"PASS: NuGet.Protocol discovery/search, prerelease={prerelease}: {string.Join(", ", versions)}");
}
