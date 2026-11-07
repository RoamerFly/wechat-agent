param(
    [Parameter(Mandatory = $true)] [string]$Launcher,
    [Parameter(Mandatory = $true)] [string]$NativeExe,
    [Parameter(Mandatory = $true)] [string]$StationExe
)

# Disposable end-to-end rehearsal for the legacy desktop hand-off.  The only
# account is created through a loopback wx instance below; this script never
# discovers, reads, or starts a real deployment.
$ErrorActionPreference = 'Stop'

function Assert-That([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
function Write-Utf8([string]$Path, [string]$Text) { [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false)) }
function Free-LoopbackPort() {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0); $listener.Start()
    try { return ([Net.IPEndPoint]$listener.LocalEndpoint).Port } finally { $listener.Stop() }
}
function Start-Child([string]$File, [string[]]$Arguments) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $File; $info.UseShellExecute = $false; $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true; $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = [Text.UTF8Encoding]::new($false); $info.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    foreach ($argument in $Arguments) { [void]$info.ArgumentList.Add($argument) }
    $process = [Diagnostics.Process]::new(); $process.StartInfo = $info
    Assert-That $process.Start() ("Could not start " + $File)
    return $process
}
function Invoke-JsonChild([string]$File, [string[]]$Arguments, [string]$Description) {
    $process = Start-Child $File $Arguments
    try {
        $process.StandardInput.Close()
        Assert-That ($process.WaitForExit(30000)) ("$Description timed out.")
        $stdout = $process.StandardOutput.ReadToEnd().Trim(); $stderr = $process.StandardError.ReadToEnd().Trim()
        Assert-That ($process.ExitCode -eq 0) ("$Description failed ($($process.ExitCode)): $stderr")
        try { return ($stdout | ConvertFrom-Json) } catch { throw "$Description returned non-JSON output: $stdout" }
    } finally { $process.Dispose() }
}
function Stop-Native([Diagnostics.Process]$Process, [string]$BaseUrl, [string]$Token, [object]$Status) {
    try {
        $body = @{ pid = [int]$Status.pid; serverId = [string]$Status.serverId; ownershipGeneration = [string]$Status.ownershipGeneration } | ConvertTo-Json -Compress
        $response = Invoke-WebRequest -UseBasicParsing -Method Post -Uri ($BaseUrl + '/v1/daemon/shutdown') -Headers @{ Authorization = 'Bearer ' + $Token } -ContentType 'application/json' -Body $body -TimeoutSec 5
        Assert-That ($response.StatusCode -eq 200) 'Synthetic wx rejected its own loopback shutdown.'
    } finally {
        Assert-That ($Process.WaitForExit(15000)) 'Synthetic wx did not stop.'
        Assert-That ($Process.ExitCode -eq 0) ('Synthetic wx stopped with ' + $Process.ExitCode)
        $Process.Dispose()
    }
}
function Get-FileBytesDigest([string]$Path) { return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() }
function Get-TreeDigest([string]$Root) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $text = [Text.StringBuilder]::new()
        foreach ($item in @(Get-ChildItem -LiteralPath $Root -Force -File -Recurse | Where-Object { $_.Name -notin @('owner.lock', 'legacy-cutover.lock', 'legacy-cutover-state.json') } | Sort-Object FullName)) {
            $relative = $item.FullName.Substring($Root.Length).TrimStart('\').Replace('\', '/')
            [void]$text.Append($relative).Append(':').Append((Get-FileBytesDigest $item.FullName)).Append("`n")
        }
        return ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($text.ToString())))).Replace('-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}
function Copy-NativeRuntime([string]$SourceExe, [string]$Target) {
    New-Item -ItemType Directory -Path $Target -Force | Out-Null
    $sourceDirectory = Split-Path -Parent $SourceExe
    # wx loads sibling DLLs.  Copy only executable/DLL payloads, never data or
    # configuration from the supplied build directory.
    foreach ($file in @(Get-ChildItem -LiteralPath $sourceDirectory -File | Where-Object { $_.Name -eq 'wx.exe' -or $_.Extension -ieq '.dll' })) {
        Copy-Item -LiteralPath $file.FullName -Destination (Join-Path $Target $file.Name) -Force
    }
    $staged = Join-Path $Target 'wx.exe'; Assert-That (Test-Path -LiteralPath $staged -PathType Leaf) 'Could not stage wx.exe.'
    $manifest = @{ schema_version = 1; platform = 'windows-x64'; native_sha256 = Get-FileBytesDigest $staged } | ConvertTo-Json -Compress
    Write-Utf8 (Join-Path $Target 'windows-native-manifest.json') $manifest
    return $staged
}
function Seed-QueuedText([string]$Database) {
    $python = (Get-Command python -ErrorAction Stop).Source
    $program = 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute("INSERT INTO outbox(id,idempotency_key,payload_hash,destination,payload,state,client_id,server_id,error_code,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)", ("queued-cutover","queued-cutover-key","queued-cutover-hash","synthetic",b"synthetic","queued","synthetic",None,None,"2026-11-07T00:00:00Z")); c.commit(); c.close()'
    & $python -c $program $Database
    if ($LASTEXITCODE -ne 0) { throw 'Could not seed the disposable queued text record.' }
}

foreach ($item in @(@{ Value = $Launcher; Name = 'Launcher'; Leaf = 'WechatAgent.exe' }, @{ Value = $NativeExe; Name = 'NativeExe'; Leaf = 'wx.exe' }, @{ Value = $StationExe; Name = 'StationExe'; Leaf = '' })) {
    if (-not [IO.Path]::IsPathFullyQualified([string]$item.Value)) { throw ("-$($item.Name) must be an absolute path.") }
    $resolved = (Resolve-Path -LiteralPath ([string]$item.Value) -ErrorAction Stop).ProviderPath
    if ($item.Leaf -and (Split-Path -Leaf $resolved) -ine $item.Leaf) { throw ("-$($item.Name) must name $($item.Leaf).") }
    Set-Variable -Name ($item.Name.ToLowerInvariant()) -Value $resolved -Scope Script
}

$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$kit = Join-Path $tempRoot ('ahs-legacy-controlled-cutover-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $kit | Out-Null
$kit = (Resolve-Path -LiteralPath $kit).ProviderPath
$complete = $false; $serve = $null

try {
    $data = Join-Path $kit 'data'; $secrets = Join-Path $kit 'secrets'; $runtime = Join-Path $kit 'runtimes\windows-x64'
    New-Item -ItemType Directory -Path $secrets | Out-Null
    $stagedWx = Copy-NativeRuntime $nativeexe $runtime
    $adminPort = Free-LoopbackPort; $nativePort = Free-LoopbackPort
    $config = @{ backend = 'windows-native'; data_root = 'data'; runtime_directory = 'runtimes/windows-x64'; account = 'synthetic'; admin_url = ('http://127.0.0.1:' + $adminPort); native_url = ('http://127.0.0.1:' + $nativePort) } | ConvertTo-Json -Compress
    Write-Utf8 (Join-Path $secrets 'local-deployment.json') $config
    $nativeConfig = Join-Path $kit 'native-server.toml'; Write-Utf8 $nativeConfig "schemaVersion = 1`n"

    # Let the real native program create its own private data root and managed
    # synthetic account.  No network request leaves 127.0.0.1.
    $serve = Start-Child $stagedWx @('--json', '--server-config', $nativeConfig, '--data-dir', $data, '--listen', ('127.0.0.1:' + $nativePort), 'serve')
    $tokenPath = Join-Path $data 'api-token'; $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while (-not (Test-Path -LiteralPath $tokenPath -PathType Leaf)) {
        if ($serve.HasExited -or [DateTime]::UtcNow -gt $deadline) { throw 'Synthetic wx did not create its loopback token.' }; Start-Sleep -Milliseconds 100
    }
    $token = [IO.File]::ReadAllText($tokenPath, [Text.UTF8Encoding]::new($false)).Trim(); $headers = @{ Authorization = 'Bearer ' + $token }
    $baseUrl = 'http://127.0.0.1:' + $nativePort; $status = $null
    while ($null -eq $status) {
        try { $status = Invoke-RestMethod -Method Get -Uri ($baseUrl + '/v1/status') -Headers $headers -TimeoutSec 2 }
        catch { if ($serve.HasExited -or [DateTime]::UtcNow -gt $deadline) { throw 'Synthetic wx did not become ready.' }; Start-Sleep -Milliseconds 100 }
    }
    $created = Invoke-WebRequest -UseBasicParsing -Method Post -Uri ($baseUrl + '/v1/accounts') -Headers $headers -ContentType 'application/json' -Body '{"id":"synthetic","alias":"synthetic"}' -TimeoutSec 5
    Assert-That ($created.StatusCode -in 200, 201, 204) 'Synthetic wx refused the synthetic account.'
    $init = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method Get -Uri ($baseUrl + '/v1/accounts/synthetic/operations/nonexistent') -Headers $headers -TimeoutSec 5
    Assert-That ($init.StatusCode -eq 404) 'Synthetic control DB was not initialized normally.'
    $serverId = [string]$status.serverId
    Stop-Native $serve $baseUrl $token $status; $serve = $null

    $registry = Join-Path $data 'registry.sqlite'; $ledger = Join-Path $data 'accounts\synthetic\agent\control.sqlite'
    Assert-That ((Test-Path -LiteralPath $registry) -and (Test-Path -LiteralPath $ledger)) 'Synthetic native root lacks the registry or account ledger.'
    $profile = Join-Path $data 'accounts\synthetic\profile'; New-Item -ItemType Directory -Path $profile -Force | Out-Null
    Write-Utf8 (Join-Path $profile 'sentinel.txt') 'synthetic profile must remain byte-identical'
    # These are legacy-owned administrative artifacts.  They represent an
    # enabled worker plus a waiting wake and are deliberately inert: preparing
    # a cutover must neither start nor rewrite them.
    $adminConfig = Join-Path $data 'admin\config.json'; New-Item -ItemType Directory -Path (Split-Path -Parent $adminConfig) -Force | Out-Null
    Write-Utf8 $adminConfig '{"enabled":true,"filehelper_enabled":true}'
    $wake = Join-Path $data 'bot-state\messages-v2.json'; New-Item -ItemType Directory -Path (Split-Path -Parent $wake) -Force | Out-Null
    Write-Utf8 $wake ('{"version":2,"server":"' + $serverId + '","account":"synthetic","conversation_wakes":[{"state":"waiting","purpose":"synthetic cutover wake"}]}')
    $beforeRoot = Get-TreeDigest $data; $beforeRegistry = Get-FileBytesDigest $registry; $beforeLedger = Get-FileBytesDigest $ledger; $beforeWake = Get-FileBytesDigest $wake; $beforeProfile = Get-FileBytesDigest (Join-Path $profile 'sentinel.txt')

    $prepared = Invoke-JsonChild $launcher @('--prepare-legacy-cutover', '--workspace', $kit) 'WechatAgent offline cutover preparation'
    Assert-That ($prepared.state -eq 'stopped' -and $prepared.mode -eq 'offline_hold') 'WechatAgent did not report an offline hold.'
    $marker = Join-Path $data 'legacy-cutover-state.json'; Assert-That (Test-Path -LiteralPath $marker -PathType Leaf) 'WechatAgent did not write its stop proof.'
    $proof = Get-Content -LiteralPath $marker -Raw | ConvertFrom-Json
    Assert-That ([int]$proof.version -eq 2 -and $proof.state -eq 'stopped' -and $proof.mode -eq 'offline_hold' -and $proof.account_id -eq 'synthetic' -and $proof.native_server_id -eq $serverId) 'Stop proof is not bound to the synthetic native dataset.'
    Assert-That ((Get-TreeDigest $data) -eq $beforeRoot) 'Preparing cutover rewrote legacy data other than its explicit stop proof.'
    Assert-That ((Get-FileBytesDigest $registry) -eq $beforeRegistry -and (Get-FileBytesDigest $ledger) -eq $beforeLedger -and (Get-FileBytesDigest $wake) -eq $beforeWake -and (Get-FileBytesDigest (Join-Path $profile 'sentinel.txt')) -eq $beforeProfile) 'Preparing cutover changed a legacy ledger, wake, or profile.'
    Assert-That (-not (Test-Path -LiteralPath (Join-Path $data 'logs'))) 'Headless cutover preparation spawned web/agent logging.'

    $snapshotParent = Join-Path $kit 'snapshots'; New-Item -ItemType Directory -Path $snapshotParent | Out-Null; $snapshot = Join-Path $snapshotParent 'synthetic-snapshot'
    $snapshotArgs = @('legacy-wechat-snapshot', '--legacy-data', $data, '--snapshot', $snapshot, '--account', 'synthetic')
    $preview = Invoke-JsonChild $stationexe $snapshotArgs 'Station snapshot preview'
    Assert-That ($preview.state -eq 'ready' -and -not [string]::IsNullOrWhiteSpace([string]$preview.apply_digest) -and -not (Test-Path -LiteralPath $snapshot)) 'Station snapshot preview wrote data or omitted its confirmation digest.'
    $captured = Invoke-JsonChild $stationexe ($snapshotArgs + @('--apply', [string]$preview.apply_digest)) 'Station snapshot apply'
    $verified = Invoke-JsonChild $stationexe @('legacy-wechat-snapshot', '--snapshot', $snapshot, '--verify') 'Station snapshot verification'
    Assert-That ($captured.state -eq 'created' -and $verified.state -eq 'verified' -and $captured.manifest_digest -eq $verified.manifest_digest) 'Station snapshot apply/verify did not agree on the manifest.'
    Assert-That ((Get-FileBytesDigest $registry) -eq $beforeRegistry -and (Get-FileBytesDigest $ledger) -eq $beforeLedger -and (Get-FileBytesDigest $wake) -eq $beforeWake -and (Get-FileBytesDigest (Join-Path $profile 'sentinel.txt')) -eq $beforeProfile) 'Station snapshot changed a legacy source file.'

    # A stopped host snapshot and a native profile export carry different data.
    # Run the actual offline import into the path the station owns, using a
    # disposable profile that has never logged in to WeChat.
    $export = Join-Path $snapshotParent 'synthetic-native-profile'
    $target = Join-Path $kit 'station-data\apps\wechat\native'
    $accountArgs = @('--json', '--account', 'synthetic')
    $copied = Invoke-JsonChild $stagedWx ($accountArgs + @('--data-dir', $data, 'store', 'export', '--destination', $export, '--source-stopped')) 'Native profile export'
    Assert-That ($copied.state -eq 'copied' -and $copied.incompleteCopy -eq $false -and $copied.daemonSidecarIncluded -eq $false -and $copied.daemonActiveCredentialsIncluded -eq $false) 'Native export did not distinguish profile bytes from daemon state.'
    Assert-That ((Get-FileBytesDigest (Join-Path $export 'sentinel.txt')) -eq $beforeProfile) 'Export changed the synthetic profile bytes.'
    $importArgs = $accountArgs + @('--data-dir', $target, 'store', 'import', '--source', $export, '--source-stopped')
    $imported = Invoke-JsonChild $stagedWx $importArgs 'Native profile import'
    $expectedProfile = Join-Path $target 'accounts\synthetic\profile'
    $receiptPath = ([string]$imported.profilePath).Replace('\\?\', '')
    Assert-That ($imported.id -eq 'synthetic' -and $imported.storageMode -eq 'managed' -and $receiptPath -eq $expectedProfile) 'Native import receipt did not bind the original account to the new managed path.'
    Assert-That ((Get-FileBytesDigest (Join-Path $expectedProfile 'sentinel.txt')) -eq $beforeProfile) 'Native import changed the profile.'
    $newLedger = Join-Path $target 'accounts\synthetic\agent\control.sqlite'
    $python = (Get-Command python -ErrorAction Stop).Source
    $inspect = @'
import pathlib
import sqlite3
import sys

uri = pathlib.Path(sys.argv[1]).as_uri() + "?mode=ro"
with sqlite3.connect(uri, uri=True) as connection:
    tables = {row[0] for row in connection.execute("select name from sqlite_schema where type='table'")}
    for name in ("outbox", "media_jobs"):
        if name in tables:
            assert connection.execute("select count(*) from " + name).fetchone()[0] == 0
'@
    & $python -B -c $inspect $newLedger
    Assert-That ($LASTEXITCODE -eq 0) 'Imported profile has an executable native delivery queue.'
    $duplicate = Start-Child $stagedWx $importArgs
    try {
        $duplicate.StandardInput.Close()
        Assert-That ($duplicate.WaitForExit(15000) -and $duplicate.ExitCode -ne 0) 'A second native import replaced an existing account.'
    } finally { $duplicate.Dispose() }
    Assert-That ((Get-FileBytesDigest $ledger) -eq $beforeLedger -and (Get-FileBytesDigest (Join-Path $expectedProfile 'sentinel.txt')) -eq $beforeProfile) 'Native migration changed the old ledger or replaced the target profile.'

    $nativeSnapshotParent = Join-Path $kit 'native-snapshots'
    New-Item -ItemType Directory -Path $nativeSnapshotParent | Out-Null
    $acl = [Security.AccessControl.DirectorySecurity]::new()
    $owner = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $acl.SetOwner($owner)
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in @($owner, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'))) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow'))
    }
    Set-Acl -LiteralPath $nativeSnapshotParent -AclObject $acl
    $nativeSnapshot = Join-Path $nativeSnapshotParent 'relocated'
    $snapshotArgs = @('--json', '--data-dir', $data, '--account', 'synthetic', 'cutover-snapshot', '--destination', $nativeSnapshot, '--source-stopped')
    $nativeReceipt = Invoke-JsonChild $stagedWx $snapshotArgs 'Native recovery state relocation'
    Assert-That ($nativeReceipt.state -eq 'copied' -and $nativeReceipt.nativeStatePreserved -eq $true -and $nativeReceipt.serverId -eq $serverId) 'Native relocation lost its original dataset.'
    Assert-That ((Get-FileBytesDigest (Join-Path $nativeSnapshot 'accounts\synthetic\agent\control.sqlite')) -eq $beforeLedger -and (Get-FileBytesDigest (Join-Path $nativeSnapshot 'accounts\synthetic\profile\sentinel.txt')) -eq $beforeProfile) 'Native relocation changed the account state or profile bytes.'
    Assert-That (-not (Test-Path -LiteralPath (Join-Path $nativeSnapshot 'api-token')) -and (Get-FileBytesDigest $ledger) -eq $beforeLedger) 'Native relocation copied an API token or changed the source.'

    # An already claimed old root must be rejected before a maintenance child
    # can be admitted.  This is a deliberate error-path check in the same kit.
    Write-Utf8 (Join-Path $data 'legacy-cutover-claim.json') '{"synthetic":true}'
    $rejected = Start-Child $launcher @('--prepare-legacy-cutover', '--workspace', $kit)
    try {
        $rejected.StandardInput.Close(); Assert-That ($rejected.WaitForExit(15000)) 'Claimed cutover rejection timed out.'
        $errorText = $rejected.StandardError.ReadToEnd()
        # .NET Framework desktop binaries write Chinese errors through the
        # active Windows console code page even with redirected streams.  The
        # stable contract for this safety guard is therefore nonzero exit and
        # no successful stop record, not lossy test-host text decoding.
        Assert-That ($rejected.ExitCode -ne 0 -and $errorText -notmatch '"state"\s*:\s*"stopped"') 'Claimed cutover was unexpectedly admitted.'
    } finally { $rejected.Dispose() }

    # Remove only the synthetic claim and inject a normal queued outbox row.
    # The real offline hold must reject this live delivery ledger before it can
    # replace the prior stopped proof; no web worker is allowed to start while
    # evaluating this error path.
    [IO.File]::Delete((Join-Path $data 'legacy-cutover-claim.json'))
    Seed-QueuedText $ledger
    $queuedLedger = Get-FileBytesDigest $ledger
    $queued = Start-Child $launcher @('--prepare-legacy-cutover', '--workspace', $kit)
    try {
        $queued.StandardInput.Close(); Assert-That ($queued.WaitForExit(30000)) 'Queued-ledger cutover rejection timed out.'
        $queuedStdout = $queued.StandardOutput.ReadToEnd(); $queuedStderr = $queued.StandardError.ReadToEnd()
        Assert-That ($queued.ExitCode -ne 0 -and $queuedStdout -notmatch '"state"\s*:\s*"stopped"') 'Queued legacy ledger was unexpectedly admitted.'
        $failedProof = Get-Content -LiteralPath $marker -Raw | ConvertFrom-Json
        Assert-That ($failedProof.state -ne 'stopped') 'Queued legacy ledger retained a stopped proof after failed validation.'
        Assert-That ((Get-FileBytesDigest $ledger) -eq $queuedLedger) 'Queued legacy ledger was modified during failed offline validation.'
        Assert-That (-not (Test-Path -LiteralPath (Join-Path $data 'logs'))) 'Queued-ledger rejection spawned web/agent logging.'
    } finally { $queued.Dispose() }

    $complete = $true
    Write-Output 'Controlled legacy WeChat cutover rehearsal passed: offline hold, host snapshot, profile export/import, and native recovery state relocation.'
} catch {
    Write-Warning "Synthetic controlled-cutover kit retained for inspection: $kit"
    throw
} finally {
    if ($serve -and -not $serve.HasExited) { try { $serve.Kill(); $serve.WaitForExit(3000) | Out-Null } catch { } }
    if ($serve) { $serve.Dispose() }
    if ($complete -and (Test-Path -LiteralPath $kit)) {
        $resolved = (Resolve-Path -LiteralPath $kit).ProviderPath
        if ($resolved.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path -Leaf $resolved) -match '^ahs-legacy-controlled-cutover-[0-9a-f]{32}$') { Remove-Item -LiteralPath $resolved -Recurse -Force }
    }
}
