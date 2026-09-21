[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Binary,
    [Parameter(Mandatory = $true)]
    [string]$Checksum
)

$ErrorActionPreference = 'Stop'
$Binary = (Resolve-Path -LiteralPath $Binary).Path
$Checksum = (Resolve-Path -LiteralPath $Checksum).Path

$want = ((Get-Content -LiteralPath $Checksum -Raw) -split '\s+')[0].ToLowerInvariant()
$got = (Get-FileHash -LiteralPath $Binary -Algorithm SHA256).Hash.ToLowerInvariant()
if ($want -ne $got) { throw 'release checksum mismatch' }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("prowl-release-smoke-" + [guid]::NewGuid())
$previousXdgCacheHome = $env:XDG_CACHE_HOME
$previousXdgConfigHome = $env:XDG_CONFIG_HOME
$previousXdgStateHome = $env:XDG_STATE_HOME
$env:XDG_CACHE_HOME = Join-Path $tmp 'cache'
$env:XDG_CONFIG_HOME = Join-Path $tmp 'config'
$env:XDG_STATE_HOME = Join-Path $tmp 'state'
$project = Join-Path $tmp 'project'
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $project '.cursor') | Out-Null
    Set-Content -LiteralPath (Join-Path $project 'main.go') -Value "package demo`n`nfunc Hello() string { return `"hello`" }`n" -NoNewline
    Set-Content -LiteralPath (Join-Path $project 'README.md') -Value "# Demo`n" -NoNewline

    Push-Location $project
    try {
        $planPath = Join-Path $tmp 'plan.json'
        & $Binary init --dry-run --json --integrations cursor,agents | Set-Content -LiteralPath $planPath -NoNewline
        $plan = Get-Content -LiteralPath $planPath -Raw | ConvertFrom-Json
        if (-not $plan.dry_run) { throw 'dry run was not reported' }
        $clientIntegrations = @($plan.plan.actions | ForEach-Object integration | Where-Object { $_ -ne 'skill' } | Sort-Object -Unique)
        if (($clientIntegrations -join ',') -ne 'agents,cursor') { throw "unexpected dry-run integrations: $($clientIntegrations -join ',')" }
        if (-not ($plan.plan.actions | Where-Object { $_.integration -eq 'skill' })) { throw 'no skill install actions in plan' }
        if (Test-Path -LiteralPath '.prowl') { throw 'dry run created .prowl' }

        $initPath = Join-Path $tmp 'init.json'
        & $Binary init --no-input --json --integrations cursor,agents | Set-Content -LiteralPath $initPath -NoNewline
        $init = Get-Content -LiteralPath $initPath -Raw | ConvertFrom-Json
        if ($init.indexed.Indexed -ne 2) { throw "indexed $($init.indexed.Indexed) files, expected 2" }
        if (-not (Test-Path -LiteralPath '.cursor/mcp.json')) { throw 'cursor integration was not created' }
        if (-not (Test-Path -LiteralPath 'AGENTS.md')) { throw 'agents integration was not created' }

        (& $Binary overview --format json | ConvertFrom-Json) | Out-Null
        (& $Binary status --json | ConvertFrom-Json) | Out-Null
        & $Binary update --help | Out-Null

        & $Binary init --remove-integrations --no-input --json --integrations cursor,agents | Out-Null
        if (Test-Path -LiteralPath 'AGENTS.md') { throw 'agents integration was not removed' }
        $servers = (Get-Content -LiteralPath '.cursor/mcp.json' -Raw | ConvertFrom-Json).mcpServers
        # Removal must drop the active renamed key; the retired legacy name stays gone too.
        if ($null -ne $servers.'prowl') { throw 'active prowl MCP server was not removed' }
        if ($null -ne $servers.'prowl-agent') { throw 'legacy prowl-agent MCP server was not removed' }
    } finally {
        Pop-Location
    }
} finally {
    $env:XDG_CACHE_HOME = $previousXdgCacheHome
    $env:XDG_CONFIG_HOME = $previousXdgConfigHome
    $env:XDG_STATE_HOME = $previousXdgStateHome
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'release smoke test passed'
