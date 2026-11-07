param([Parameter(Mandatory=$true)][string]$PackageDirectory)

$ErrorActionPreference = 'Stop'
$package = (Resolve-Path -LiteralPath $PackageDirectory).ProviderPath
$runner = Join-Path $package 'DesktopTests.exe'
$station = Join-Path $package 'agent-home-station-server.exe'
if (-not (Test-Path -LiteralPath $runner -PathType Leaf) -or -not (Test-Path -LiteralPath $station -PathType Leaf)) {
    throw 'Build the Windows LocalStation package with -Test before the rehearsal.'
}

$testBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$kit = Join-Path $testBase ('ahs-wechat-cutover-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $kit | Out-Null
$workspace = Split-Path $PSScriptRoot -Parent
$priorOS = $env:GOOS
$priorArch = $env:GOARCH
Push-Location $workspace
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    & go build -trimpath -o (Join-Path $kit 'fixture-wx.exe') ./scripts/testdata/station-cutover-native
    if ($LASTEXITCODE -ne 0) { throw 'Synthetic native transport build failed.' }
    & go build -trimpath -o (Join-Path $kit 'fixture-admin.exe') ./cmd/wechat-admin
    if ($LASTEXITCODE -ne 0) { throw 'Legacy management service build failed.' }
    & $runner --wechat-cutover-rehearsal $station $kit
    if ($LASTEXITCODE -ne 0) { throw 'Windows cutover rehearsal failed.' }
    $resolvedKit = (Resolve-Path -LiteralPath $kit).ProviderPath
    if (-not $resolvedKit.StartsWith($testBase, [StringComparison]::OrdinalIgnoreCase) -or
        (Split-Path $resolvedKit -Leaf) -notmatch '^ahs-wechat-cutover-[0-9a-f]{32}$') {
        throw 'Unexpected test path; cleanup refused.'
    }
    Remove-Item -LiteralPath $resolvedKit -Recurse -Force
} catch {
    Write-Warning "Synthetic rehearsal directory retained for inspection: $kit"
    throw
} finally {
    Pop-Location
    $env:GOOS = $priorOS
    $env:GOARCH = $priorArch
}
