[CmdletBinding()]
param()

# Exercise install.ps1 end to end against a local loopback release, proving the
# renamed prowl.exe lands and the retired prowl-agent.exe sibling is cleaned from
# the installer's own destination only -- a prowl-agent elsewhere is never
# touched, a repeat install stays idempotent, and a legacy sibling that cannot be
# removed (held open) surfaces an actionable warning without aborting the install.
# PowerShell 7 dropped file:// from Invoke-WebRequest, so the stand-in release is
# served over http://localhost.
$ErrorActionPreference = 'Stop'

$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$installer = Join-Path $repo 'install.ps1'
if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) { throw "install.ps1 not found at $installer" }

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { $arch = 'amd64' }
    default { throw "installer smoke unsupported on $($_)" }
}
$name = "prowl-windows-$arch.exe"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("prowl-installer-smoke-" + [guid]::NewGuid())
$release = Join-Path $tmp 'release'
$dest = Join-Path $tmp 'bin'
$elsewhere = Join-Path $tmp 'other'
New-Item -ItemType Directory -Force -Path $release, $dest, $elsewhere | Out-Null

# A stand-in release artifact plus its checksum.
$artifact = Join-Path $release $name
Set-Content -LiteralPath $artifact -Value 'prowl-stub' -NoNewline
$hash = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$artifact.sha256" -Value "$hash  $name" -NoNewline

# A stale legacy sibling in the install destination (must be removed) and an
# unrelated prowl-agent elsewhere (must survive).
Set-Content -LiteralPath (Join-Path $dest 'prowl-agent.exe') -Value 'stale-legacy' -NoNewline
Set-Content -LiteralPath (Join-Path $elsewhere 'prowl-agent.exe') -Value 'unrelated' -NoNewline

# Pick a free loopback port. localhost prefixes need no URL reservation.
$probe = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$probe.Start()
$port = ([System.Net.IPEndPoint]$probe.LocalEndpoint).Port
$probe.Stop()
$base = "http://localhost:$port"
$prefix = "$base/"

$server = Start-Job -ScriptBlock {
    param($prefix, $root)
    $listener = [System.Net.HttpListener]::new()
    $listener.Prefixes.Add($prefix)
    $listener.Start()
    try {
        while ($true) {
            $ctx = $listener.GetContext()
            $rel = [uri]::UnescapeDataString($ctx.Request.Url.AbsolutePath).TrimStart('/')
            if ($rel -eq '__shutdown') {
                $ctx.Response.StatusCode = 200
                $ctx.Response.Close()
                break
            }
            $path = Join-Path $root $rel
            if (Test-Path -LiteralPath $path -PathType Leaf) {
                $bytes = [System.IO.File]::ReadAllBytes($path)
                $ctx.Response.StatusCode = 200
                $ctx.Response.ContentLength64 = $bytes.Length
                $ctx.Response.OutputStream.Write($bytes, 0, $bytes.Length)
            } else {
                $ctx.Response.StatusCode = 404
            }
            $ctx.Response.Close()
        }
    } finally {
        $listener.Stop()
    }
} -ArgumentList $prefix, $release

$previousBase = $env:PROWL_RELEASE_BASE
$previousDir = $env:PROWL_INSTALL_DIR
try {
    # Wait for the loopback server to accept requests.
    $ready = $false
    for ($i = 0; $i -lt 50 -and -not $ready; $i++) {
        try {
            Invoke-WebRequest "$base/$name.sha256" -OutFile ([System.IO.Path]::GetTempFileName()) -UseBasicParsing | Out-Null
            $ready = $true
        } catch {
            Start-Sleep -Milliseconds 200
        }
    }
    if (-not $ready) { throw 'loopback release server did not become ready' }

    $env:PROWL_RELEASE_BASE = $base
    $env:PROWL_INSTALL_DIR = $dest

    # First install: renamed binary lands, stale legacy sibling is gone, and the
    # prowl-agent outside the destination is left alone.
    & $installer | Out-Null
    if (-not (Test-Path -LiteralPath (Join-Path $dest 'prowl.exe') -PathType Leaf)) { throw "install did not place prowl.exe in $dest" }
    if (Test-Path -LiteralPath (Join-Path $dest 'prowl-agent.exe')) { throw 'stale legacy prowl-agent.exe survived install' }
    if (-not (Test-Path -LiteralPath (Join-Path $elsewhere 'prowl-agent.exe'))) { throw 'installer removed a prowl-agent outside its destination' }

    # Second install with no legacy present: idempotent, no error, prowl still there.
    & $installer | Out-Null
    if (-not (Test-Path -LiteralPath (Join-Path $dest 'prowl.exe') -PathType Leaf)) { throw 'second install lost prowl.exe' }
    if (Test-Path -LiteralPath (Join-Path $dest 'prowl-agent.exe')) { throw 'second install resurrected prowl-agent.exe' }

    # Third install: a legacy sibling that cannot be removed (a handle holds it
    # open with no sharing) must not silently succeed. The installer surfaces an
    # actionable warning and still lands prowl.exe rather than aborting.
    $locked = Join-Path $dest 'prowl-agent.exe'
    Set-Content -LiteralPath $locked -Value 'stale-locked' -NoNewline
    $handle = [System.IO.File]::Open($locked, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::None)
    try {
        $captured = & $installer 3>&1 | Out-String
        if ($captured -notmatch 'Could not remove the retired') { throw "an un-removable legacy was cleaned up silently: $captured" }
        if (-not (Test-Path -LiteralPath (Join-Path $dest 'prowl.exe') -PathType Leaf)) { throw 'a cleanup failure aborted the install' }
        if (-not (Test-Path -LiteralPath $locked -PathType Leaf)) { throw 'the un-removable legacy unexpectedly vanished' }
    } finally {
        $handle.Close()
        $handle.Dispose()
    }
} finally {
    $env:PROWL_RELEASE_BASE = $previousBase
    $env:PROWL_INSTALL_DIR = $previousDir
    try { Invoke-WebRequest "$base/__shutdown" -UseBasicParsing -TimeoutSec 5 | Out-Null } catch {}
    Stop-Job $server -ErrorAction SilentlyContinue
    Remove-Job $server -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'installer smoke test passed'
