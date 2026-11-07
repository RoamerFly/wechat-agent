param(
    [Parameter(Mandatory = $true)]
    [string]$CandidateLauncher
)

# This rehearsal deliberately never opens a real desktop package, starts an
# executable, or reaches a real WeChat runtime.  It proves the launcher
# replacement contract against a disposable legacy-shaped directory only.
$ErrorActionPreference = 'Stop'

function Assert-That([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Get-Sha256([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Write-Utf8([string]$Path, [string]$Text) {
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Invoke-Upgrade([System.Collections.IDictionary]$UpgradeParameters) {
    try {
        $raw = @(& $script:upgrade @UpgradeParameters 2>&1)
        $code = $LASTEXITCODE
    } catch {
        $raw = @($_)
        $code = 1
    }
    return @{ output = ($raw | Out-String); code = $code }
}

function Read-UpgradeJson([System.Collections.IDictionary]$UpgradeParameters) {
    $invocation = Invoke-Upgrade $UpgradeParameters
    if ($invocation.code -ne 0) {
        throw ("Upgrade command failed:`n" + $invocation.output)
    }
    $json = $invocation.output.Trim()
    try { return $json | ConvertFrom-Json }
    catch { throw ("Upgrade command did not return JSON:`n" + $json) }
}

function Assert-UpgradeRejected([string]$Name, [System.Collections.IDictionary]$UpgradeParameters, [string]$ExpectedText) {
    $invocation = Invoke-Upgrade $UpgradeParameters
    if ($invocation.code -eq 0) {
        throw ("Expected upgrade guard to reject " + $Name + ". Output:`n" + $invocation.output)
    }
    Assert-That ($invocation.output -match [Regex]::Escape($ExpectedText)) ("Wrong rejection for " + $Name + ". Output:`n" + $invocation.output)
}

function Assert-PathUnderKit([string]$Path, [string]$Description) {
    $full = [IO.Path]::GetFullPath($Path)
    Assert-That ($full.StartsWith($script:kitRoot + '\', [StringComparison]::OrdinalIgnoreCase)) "$Description escaped the synthetic rehearsal directory."
    return $full
}

function Copy-UpgradeParameters([System.Collections.IDictionary]$Source) {
    $copy = @{}
    foreach ($entry in $Source.GetEnumerator()) { $copy[$entry.Key] = $entry.Value }
    return $copy
}

$workspace = Split-Path -Parent $PSScriptRoot
$upgrade = Join-Path $PSScriptRoot 'Update-LegacyWechatLauncher.ps1'
if (-not (Test-Path -LiteralPath $upgrade -PathType Leaf)) {
    throw 'Update-LegacyWechatLauncher.ps1 is missing. Build the launcher update command before running its rehearsal.'
}

if (-not [IO.Path]::IsPathFullyQualified($CandidateLauncher)) {
    throw '-CandidateLauncher must be an absolute path to a freshly built WechatAgent.exe.'
}
$candidate = (Resolve-Path -LiteralPath $CandidateLauncher -ErrorAction Stop).ProviderPath
if (-not (Test-Path -LiteralPath $candidate -PathType Leaf) -or [IO.Path]::GetFileName($candidate) -ine 'WechatAgent.exe') {
    throw '-CandidateLauncher must name an existing WechatAgent.exe file.'
}

$candidateHash = Get-Sha256 $candidate
$testBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$kit = Join-Path $testBase ('ahs-legacy-launcher-upgrade-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $kit | Out-Null
$kitRoot = (Resolve-Path -LiteralPath $kit).ProviderPath
$completed = $false

try {
    $oldWorkspace = Join-Path $kitRoot 'legacy-desktop'
    $runtime = Join-Path $oldWorkspace 'runtimes\windows-x64'
    $dataRoot = Join-Path $oldWorkspace 'data'
    $secrets = Join-Path $oldWorkspace 'secrets'
    New-Item -ItemType Directory -Force -Path $runtime, $dataRoot, $secrets | Out-Null

    # Compile a non-executed pre-interlock launcher stub in the temporary kit.
    # Its deliberately absent interlock marker exercises archival of an old
    # stop proof during the one-way update, without fabricating a malformed PE.
    $csc = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
    Assert-That (Test-Path -LiteralPath $csc -PathType Leaf) 'The .NET Framework C# compiler is required for the isolated launcher rehearsal.'
    $legacyStub = Join-Path $kitRoot 'LegacyLauncherStub.cs'
    Write-Utf8 $legacyStub 'namespace WechatAgent.Desktop { internal static class LegacyLauncherStub { static int Main() { return 0; } } }'
    $launcher = Join-Path $oldWorkspace 'WechatAgent.exe'
    & $csc /nologo /target:exe /main:WechatAgent.Desktop.LegacyLauncherStub ('/out:' + $launcher) /optimize- $legacyStub
    if ($LASTEXITCODE -ne 0) { throw 'Synthetic legacy WechatAgent.exe compilation failed.' }
    $oldHash = Get-Sha256 $launcher
    Assert-That ($oldHash -ne $candidateHash) 'The synthetic legacy launcher unexpectedly matches the candidate hash.'

    # Tiny placeholder files are enough here: the update command must verify
    # their manifest identities, never launch them during a handoff.
    $wx = Join-Path $runtime 'wx.exe'
    $admin = Join-Path $runtime 'wechat-admin.exe'
    Write-Utf8 $wx 'synthetic wx runtime - never executable'
    Write-Utf8 $admin 'synthetic admin runtime - never executable'
    $runtimeManifest = [ordered]@{
        schema_version = 1
        platform = 'windows-x64'
        native_sha256 = Get-Sha256 $wx
        admin_sha256 = Get-Sha256 $admin
        files = [ordered]@{
            'wx.exe' = Get-Sha256 $wx
            'wechat-admin.exe' = Get-Sha256 $admin
        }
    } | ConvertTo-Json -Depth 5
    Write-Utf8 (Join-Path $runtime 'windows-native-manifest.json') $runtimeManifest
    $nativeSentinel = Join-Path $runtime 'preserve-native-sentinel.txt'
    $dataSentinel = Join-Path $dataRoot 'preserve-data-sentinel.txt'
    Write-Utf8 $nativeSentinel 'native sentinel must survive every rehearsal phase'
    Write-Utf8 $dataSentinel 'data sentinel must survive every rehearsal phase'
    $nativeSentinelHash = Get-Sha256 $nativeSentinel
    $dataSentinelHash = Get-Sha256 $dataSentinel

    $statePath = Join-Path $dataRoot 'legacy-cutover-state.json'
    $oldState = [ordered]@{
        version = 2
        state = 'stopped'
        epoch = 7
        account_id = 'synthetic_legacy'
        data_root = $dataRoot
        native_server_id = 'synthetic-native'
        at = '2026-11-07T00:00:00Z'
    } | ConvertTo-Json -Depth 4
    Write-Utf8 $statePath $oldState
    $oldStateBytes = [IO.File]::ReadAllBytes($statePath)
    $deployment = [ordered]@{
        backend = 'windows-native'
        data_root = 'data'
        runtime_directory = 'runtimes/windows-x64'
        account = 'synthetic_legacy'
        admin_url = 'http://127.0.0.1:58181'
        native_url = 'http://127.0.0.1:58182'
    } | ConvertTo-Json -Depth 4
    $deploymentPath = Join-Path $secrets 'local-deployment.json'
    Write-Utf8 $deploymentPath $deployment
    $deploymentBytes = [IO.File]::ReadAllBytes($deploymentPath)

    $base = [ordered]@{
        Workspace = $oldWorkspace
        Launcher = $launcher
        Replacement = $candidate
        ReplacementSha256 = $candidateHash
    }

    $preview = Read-UpgradeJson -UpgradeParameters $base
    Assert-That ($preview.state -eq 'ready') 'Preview did not return state=ready.'
    Assert-That (-not [string]::IsNullOrWhiteSpace([string]$preview.apply_digest)) 'Preview did not return an apply_digest.'
    Assert-That ((Get-Sha256 $launcher) -eq $oldHash) 'Preview modified the legacy launcher.'
    Assert-That ([IO.File]::Exists($statePath)) 'Preview modified the legacy stop proof.'
    Assert-That ((Get-Sha256 $nativeSentinel) -eq $nativeSentinelHash) 'Preview modified the native runtime sentinel.'
    Assert-That ((Get-Sha256 $dataSentinel) -eq $dataSentinelHash) 'Preview modified the legacy data sentinel.'

    $stale = Copy-UpgradeParameters $base; $stale.ApplyDigest = ('0' * 64)
    Assert-UpgradeRejected -Name 'stale apply digest' -UpgradeParameters $stale -ExpectedText '预演摘要已经变化'
    Assert-That ((Get-Sha256 $launcher) -eq $oldHash) 'A stale apply digest modified the legacy launcher.'

    # A real preview token is also rejected if a checked input changed after
    # preview.  Restore exact bytes afterwards so later phases do not inherit
    # a synthetic drift condition.
    $drifted = $deployment -replace 'synthetic_legacy', 'synthetic_changed'
    Write-Utf8 $deploymentPath $drifted
    $drift = Copy-UpgradeParameters $base; $drift.ApplyDigest = [string]$preview.apply_digest
    Assert-UpgradeRejected -Name 'configuration drift after preview' -UpgradeParameters $drift -ExpectedText '预演摘要已经变化'
    [IO.File]::WriteAllBytes($deploymentPath, $deploymentBytes)
    Assert-That ([Linq.Enumerable]::SequenceEqual([byte[]]$deploymentBytes, [IO.File]::ReadAllBytes($deploymentPath))) 'Could not restore the exact synthetic deployment configuration after drift testing.'

    $claim = Join-Path $dataRoot 'legacy-cutover-claim.json'
    Write-Utf8 $claim '{"synthetic":true}'
    Assert-UpgradeRejected -Name 'active legacy claim' -UpgradeParameters $base -ExpectedText '微信交接声明仍有效'
    Remove-Item -LiteralPath $claim -Force

    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 58182)
    $listener.Start()
    try { Assert-UpgradeRejected -Name 'active native service port' -UpgradeParameters $base -ExpectedText '旧端服务端口仍被占用' }
    finally { $listener.Stop() }

    # The file lock is held without opening a service or launcher.  It proves
    # the data-root active-owner guard with the same byte range used by the
    # updater; the port case above separately covers active-service rejection.
    $lockPath = Join-Path $dataRoot 'legacy-cutover.lock'
    Write-Utf8 $lockPath 'synthetic lock'
    $heldLock = [IO.File]::Open($lockPath, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::ReadWrite)
    $heldLock.Lock(0, 1)
    try { Assert-UpgradeRejected -Name 'active legacy data lock' -UpgradeParameters $base -ExpectedText '旧端运行锁仍被占用' }
    finally { $heldLock.Unlock(0, 1); $heldLock.Dispose(); Remove-Item -LiteralPath $lockPath -Force }

    $applyParameters = Copy-UpgradeParameters $base; $applyParameters.ApplyDigest = [string]$preview.apply_digest
    $apply = Read-UpgradeJson -UpgradeParameters $applyParameters
    Assert-That ($apply.state -eq 'updated') 'Apply did not return state=updated.'
    Assert-That ([string]$apply.previous_sha256 -eq $oldHash) 'Updated receipt has the wrong previous_sha256.'
    Assert-That ([string]$apply.replacement_sha256 -eq $candidateHash) 'Updated receipt has the wrong replacement_sha256.'
    $rollback = Assert-PathUnderKit ([string]$apply.rollback_file) 'Updated rollback file'
    Assert-That (Test-Path -LiteralPath $rollback -PathType Leaf) 'Updated receipt rollback_file does not exist.'
    $invalidatedProof = Assert-PathUnderKit ([string]$apply.invalidated_stop_proof) 'Updated invalidated stop proof'
    Assert-That (Test-Path -LiteralPath $invalidatedProof -PathType Leaf) 'Updated receipt invalidated_stop_proof does not exist.'
    Assert-That ((Get-Sha256 $rollback) -eq $oldHash) 'Rollback launcher hash does not preserve the old launcher.'
    Assert-That ((Get-Sha256 $launcher) -eq $candidateHash) 'Apply did not install the exact candidate launcher.'
    Assert-That (-not [IO.File]::Exists($statePath)) 'Apply did not invalidate the pre-interlock stop proof.'
    Assert-That ([Linq.Enumerable]::SequenceEqual([byte[]]$oldStateBytes, [IO.File]::ReadAllBytes($invalidatedProof))) 'Update changed the archived pre-interlock stop proof.'
    Assert-That ((Get-Sha256 $nativeSentinel) -eq $nativeSentinelHash) 'Apply changed the native runtime sentinel.'
    Assert-That ((Get-Sha256 $dataSentinel) -eq $dataSentinelHash) 'Apply changed the legacy data sentinel.'

    $restoreBase = [ordered]@{
        Workspace = $oldWorkspace
        Launcher = $launcher
        Replacement = $rollback
        ReplacementSha256 = $oldHash
        Restore = $true
    }
    $restorePreview = Read-UpgradeJson -UpgradeParameters $restoreBase
    Assert-That ($restorePreview.state -eq 'ready') 'Restore preview did not return state=ready.'
    Assert-That (-not [string]::IsNullOrWhiteSpace([string]$restorePreview.apply_digest)) 'Restore preview did not return an apply_digest.'
    $restoreParameters = Copy-UpgradeParameters $restoreBase; $restoreParameters.ApplyDigest = [string]$restorePreview.apply_digest
    $restored = Read-UpgradeJson -UpgradeParameters $restoreParameters
    Assert-That ($restored.state -eq 'restored') 'Restore did not return state=restored.'
    Assert-That ((Get-Sha256 $launcher) -eq $oldHash) 'Restore did not reinstall the original launcher.'
    Assert-That ([string]::IsNullOrWhiteSpace([string]$restored.invalidated_stop_proof)) 'Restore should not archive a stop proof already invalidated during the pre-interlock update.'
    Assert-That (-not [IO.File]::Exists($statePath)) 'Restore left the old stop proof valid after replacing the launcher.'
    Assert-That ((Get-Sha256 $nativeSentinel) -eq $nativeSentinelHash) 'Restore changed the native runtime sentinel.'
    Assert-That ((Get-Sha256 $dataSentinel) -eq $dataSentinelHash) 'Restore changed the legacy data sentinel.'
    $completed = $true
    Write-Output 'Legacy WechatAgent launcher upgrade rehearsal passed: preview, guards, update, and restore used synthetic files only.'
} catch {
    Write-Warning "Synthetic launcher rehearsal directory retained for inspection: $kitRoot"
    throw
} finally {
    if ($completed -and (Test-Path -LiteralPath $kitRoot)) {
        $resolvedKit = (Resolve-Path -LiteralPath $kitRoot).ProviderPath
        if ($resolvedKit.StartsWith($testBase, [StringComparison]::OrdinalIgnoreCase) -and
            (Split-Path -Leaf $resolvedKit) -match '^ahs-legacy-launcher-upgrade-[0-9a-f]{32}$') {
            Remove-Item -LiteralPath $resolvedKit -Recurse -Force
        }
    }
}
