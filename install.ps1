$ErrorActionPreference = 'Stop'

$repo = 'neur0map/prowl'
$base = if ($env:PROWL_RELEASE_BASE) { $env:PROWL_RELEASE_BASE } else { "https://github.com/$repo/releases/download/stable" }
$dest = if ($env:PROWL_INSTALL_DIR) { $env:PROWL_INSTALL_DIR } else { Join-Path $HOME '.local\bin' }

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { $arch = 'amd64' }
    default { throw "Unsupported Windows architecture: $($_). Build from source instead." }
}

$name = "prowl-windows-$arch.exe"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("prowl-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $binary = Join-Path $tmp $name
    $checksum = "$binary.sha256"
    Invoke-WebRequest "$base/$name" -OutFile $binary
    Invoke-WebRequest "$base/$name.sha256" -OutFile $checksum

    $want = ((Get-Content $checksum -Raw) -split '\s+')[0].ToLowerInvariant()
    $got = (Get-FileHash -Algorithm SHA256 $binary).Hash.ToLowerInvariant()
    if ($want -ne $got) { throw 'Checksum mismatch; aborting.' }

    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item $binary (Join-Path $dest 'prowl.exe') -Force

    # The transitional prowl-agent command is retired. Now that prowl.exe is in
    # place, remove the legacy sibling from this installer's own destination only
    # -- a prowl-agent elsewhere on PATH is never touched. The leaf-only test
    # keeps a repeat install idempotent; a removal that fails (a running
    # prowl-agent.exe holds the file open, or the destination is read-only) is
    # surfaced as an actionable warning instead of being silently left behind to
    # shadow prowl.exe on PATH.
    $legacy = Join-Path $dest 'prowl-agent.exe'
    if (Test-Path -LiteralPath $legacy -PathType Leaf) {
        try {
            Remove-Item -LiteralPath $legacy -Force -ErrorAction Stop
        } catch {
            Write-Warning "Could not remove the retired $legacy ($($_.Exception.Message)). Close any running prowl-agent and delete it manually; until then it can shadow prowl.exe on PATH."
        }
    }
    Write-Host "Prowl installed to $(Join-Path $dest 'prowl.exe')"
    Write-Host 'Next: cd <project>; prowl init'
    if (($env:Path -split ';') -notcontains $dest) {
        Write-Host "Note: add $dest to your user PATH."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
