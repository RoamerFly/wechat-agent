param(
    [Parameter(Mandatory)][string]$LegacyLauncher,
    [Parameter(Mandatory)][string]$LegacyNativeExe,
    [Parameter(Mandatory)][string]$CandidateLauncher,
    [Parameter(Mandatory)][string]$CandidateNativeExe,
    [Parameter(Mandatory)][string]$StationExe
)

# All writes stay in a fresh synthetic kit. Supplied executables are sources.
$ErrorActionPreference = 'Stop'

function Assert-That([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Write-Utf8([string]$Path, [string]$Text) {
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Sha256([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Free-LoopbackPort {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return ([Net.IPEndPoint]$listener.LocalEndpoint).Port }
    finally { $listener.Stop() }
}

function Start-Child([string]$File, [string[]]$Arguments) {
    $info = [Diagnostics.ProcessStartInfo]::new($File)
    $info.WorkingDirectory = [IO.Path]::GetDirectoryName($File)
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = [Text.UTF8Encoding]::new($false)
    $info.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    foreach ($name in @($info.Environment.Keys)) {
        if ($name -match '^(WECHAT_|XDG_|AHS_)') { [void]$info.Environment.Remove($name) }
    }
    foreach ($argument in $Arguments) { $info.ArgumentList.Add($argument) }
    return [Diagnostics.Process]::Start($info)
}

function Invoke-Json([string]$File, [string[]]$Arguments, [string]$Description) {
    $process = Start-Child $File $Arguments
    try {
        $process.StandardInput.Close()
        $output = $process.StandardOutput.ReadToEndAsync()
        $diagnostics = $process.StandardError.ReadToEndAsync()
        Assert-That ($process.WaitForExit(30000)) "$Description timed out."
        $errorText = $diagnostics.GetAwaiter().GetResult().Trim()
        Assert-That ($process.ExitCode -eq 0) "$Description failed ($($process.ExitCode)): $errorText"
        return ($output.GetAwaiter().GetResult() | ConvertFrom-Json)
    } finally {
        if (-not $process.HasExited) { $process.Kill(); [void]$process.WaitForExit(3000) }
        $process.Dispose()
    }
}

function Normal-RuntimeFingerprint([string]$Runtime) {
    # The new package is a child directory. Every existing top-level runtime
    # file, including the original manifest and DLLs, must stay byte identical.
    $files = Get-ChildItem -LiteralPath $Runtime -Force -File | Sort-Object Name
    return (($files | ForEach-Object { "$($_.Name)=$(Sha256 $_.FullName)" }) -join [Environment]::NewLine)
}

function Invoke-Updater([hashtable]$Parameters) {
    return (& $updater @Parameters | ConvertFrom-Json)
}

$resolved = @{}
foreach ($name in @('LegacyLauncher', 'LegacyNativeExe', 'CandidateLauncher', 'CandidateNativeExe', 'StationExe')) {
    $path = Get-Variable -Name $name -ValueOnly
    Assert-That ([IO.Path]::IsPathFullyQualified($path)) "-$name must be absolute."
    $file = Get-Item -LiteralPath $path
    Assert-That (-not $file.PSIsContainer) "-$name must be an executable file."
    $resolved[$name] = $file.FullName
}
foreach ($name in @('LegacyLauncher', 'CandidateLauncher')) {
    Assert-That ((Split-Path -Leaf $resolved[$name]) -ieq 'WechatAgent.exe') "-$name must be WechatAgent.exe."
}
foreach ($name in @('LegacyNativeExe', 'CandidateNativeExe')) {
    Assert-That ((Split-Path -Leaf $resolved[$name]) -ieq 'wx.exe') "-$name must be wx.exe."
}

$updater = Join-Path $PSScriptRoot 'Update-LegacyWechatLauncher.ps1'
$temp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$kit = Join-Path $temp ('ahs-paired-update-' + [Guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $kit
$kit = (Resolve-Path -LiteralPath $kit).ProviderPath
$complete = $false
$serve = $null
try {
    $runtime = Join-Path $kit 'runtimes/windows-x64'
    $secrets = Join-Path $kit 'secrets'
    $null = New-Item -ItemType Directory -Path $runtime, $secrets -Force
    $oldWx = Join-Path $runtime 'wx.exe'
    $launcher = Join-Path $kit 'WechatAgent.exe'
    Copy-Item -LiteralPath $resolved.LegacyNativeExe -Destination $oldWx
    Copy-Item -LiteralPath $resolved.LegacyLauncher -Destination $launcher
    $nativeSource = Split-Path -Parent $resolved.LegacyNativeExe
    foreach ($dll in (Get-ChildItem -LiteralPath $nativeSource -Filter '*.dll' -File)) {
        Copy-Item -LiteralPath $dll.FullName -Destination $runtime
    }
    $adminExe = Join-Path $runtime 'wechat-admin.exe'
    Write-Utf8 $adminExe 'synthetic unlaunchable admin placeholder'
    $oldManifest = @{
        schema_version = 1
        platform = 'windows-x64'
        native_sha256 = (Sha256 $oldWx)
        admin_sha256 = (Sha256 $adminExe)
    }
    Write-Utf8 (Join-Path $runtime 'windows-native-manifest.json') ($oldManifest | ConvertTo-Json -Compress)

    $adminPort = Free-LoopbackPort
    do { $nativePort = Free-LoopbackPort } while ($nativePort -eq $adminPort)
    $deployment = @{
        backend = 'windows-native'
        data_root = 'data'
        runtime_directory = 'runtimes/windows-x64'
        account = 'synthetic'
        admin_url = "http://127.0.0.1:$adminPort"
        native_url = "http://127.0.0.1:$nativePort"
    }
    Write-Utf8 (Join-Path $secrets 'local-deployment.json') ($deployment | ConvertTo-Json -Compress)
    $data = Join-Path $kit 'data'
    $nativeConfig = Join-Path $kit 'native-server.toml'
    Write-Utf8 $nativeConfig "schemaVersion = 1`n"
    $url = $deployment.native_url

    # Start the copied old native only on a new empty test root. No login.
    $serve = Start-Child $oldWx @('--json', '--server-config', $nativeConfig, '--data-dir', $data, '--listen', "127.0.0.1:$nativePort", 'serve')
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    $tokenFile = Join-Path $data 'api-token'
    while (-not (Test-Path -LiteralPath $tokenFile)) {
        if ($serve.HasExited) { throw "Synthetic wx exited: $($serve.StandardError.ReadToEnd())" }
        Assert-That ([DateTime]::UtcNow -lt $deadline) 'Synthetic wx did not initialize within 20 seconds.'
        Start-Sleep -Milliseconds 100
    }
    $headers = @{ Authorization = 'Bearer ' + [IO.File]::ReadAllText($tokenFile).Trim() }
    $status = $null
    while ($null -eq $status) {
        try { $status = Invoke-RestMethod -Uri "$url/v1/status" -Headers $headers -TimeoutSec 2 }
        catch {
            if ($serve.HasExited) { throw "Synthetic wx exited: $($serve.StandardError.ReadToEnd())" }
            Assert-That ([DateTime]::UtcNow -lt $deadline) 'Synthetic wx did not become ready.'
            Start-Sleep -Milliseconds 100
        }
    }
    $request = @{ Method = 'Post'; Headers = $headers; ContentType = 'application/json'; TimeoutSec = 5 }
    $account = Invoke-WebRequest @request -Uri "$url/v1/accounts" -Body '{"id":"synthetic","alias":"synthetic"}'
    Assert-That ($account.StatusCode -in 200, 201, 204) 'Synthetic account creation failed.'
    $shutdown = @{ pid = $status.pid; serverId = $status.serverId; ownershipGeneration = $status.ownershipGeneration }
    $response = Invoke-WebRequest @request -Uri "$url/v1/daemon/shutdown" -Body ($shutdown | ConvertTo-Json -Compress)
    Assert-That ($response.StatusCode -eq 200 -and $serve.WaitForExit(15000)) 'Synthetic wx did not stop.'
    Assert-That ($serve.ExitCode -eq 0) 'Synthetic wx exited unsuccessfully.'
    $serve.Dispose()
    $serve = $null

    $legacyAdmin = Join-Path $data 'admin'
    $null = New-Item -ItemType Directory -Path $legacyAdmin
    Write-Utf8 (Join-Path $legacyAdmin 'config.json') '{"enabled":true}'
    $registry = Join-Path $data 'registry.sqlite'
    $registryHash = Sha256 $registry
    $runtimeFingerprint = Normal-RuntimeFingerprint $runtime
    $nativeHash = Sha256 $resolved.CandidateNativeExe
    $update = @{
        Workspace = $kit
        Launcher = $launcher
        Replacement = $resolved.CandidateLauncher
        ReplacementSha256 = (Sha256 $resolved.CandidateLauncher)
        NativeReplacement = $resolved.CandidateNativeExe
        NativeReplacementSha256 = $nativeHash
    }
    $preview = Invoke-Updater $update
    Assert-That ($preview.state -eq 'ready' -and $preview.native_replacement_sha256 -eq $nativeHash) 'Preview omitted the native binding.'
    Assert-That (-not (Test-Path -LiteralPath $preview.paired_runtime)) 'Preview wrote a paired runtime.'
    Assert-That ((Normal-RuntimeFingerprint $runtime) -eq $runtimeFingerprint) 'Preview modified the original runtime.'

    $update.ApplyDigest = $preview.apply_digest
    $receipt = Invoke-Updater $update
    Assert-That ($receipt.state -eq 'updated' -and (Test-Path -LiteralPath $receipt.paired_runtime)) 'Apply did not publish the paired runtime.'
    Assert-That ((Normal-RuntimeFingerprint $runtime) -eq $runtimeFingerprint) 'Apply modified the original runtime.'
    $prepared = Invoke-Json $launcher @('--prepare-legacy-cutover', '--workspace', $kit) 'Paired installed launcher'
    Assert-That ($prepared.state -eq 'stopped' -and $prepared.mode -eq 'offline_hold') 'The installed launcher could not use its pair.'

    $snapshot = Join-Path $kit 'snapshot'
    $snapshotArguments = @('legacy-wechat-snapshot', '--legacy-data', $data, '--snapshot', $snapshot, '--account', 'synthetic')
    $plan = Invoke-Json $resolved.StationExe $snapshotArguments 'Snapshot preview'
    $created = Invoke-Json $resolved.StationExe ($snapshotArguments + @('--apply', $plan.apply_digest)) 'Snapshot apply'
    $verified = Invoke-Json $resolved.StationExe @('legacy-wechat-snapshot', '--snapshot', $snapshot, '--verify') 'Snapshot verify'
    Assert-That ($created.state -eq 'created' -and $verified.state -eq 'verified') 'Paired-update snapshot could not be verified.'
    Assert-That ($created.manifest_digest -eq $verified.manifest_digest) 'Snapshot manifest digest changed.'

    $restore = @{
        Workspace = $kit
        Launcher = $launcher
        Replacement = $receipt.rollback_file
        ReplacementSha256 = $receipt.previous_sha256
        Restore = $true
    }
    $restorePlan = Invoke-Updater $restore
    $restore.ApplyDigest = $restorePlan.apply_digest
    $restored = Invoke-Updater $restore
    Assert-That ($restored.state -eq 'restored' -and (Sha256 $launcher) -eq $receipt.previous_sha256) 'Restore did not restore the old launcher.'
    Assert-That (Test-Path -LiteralPath $receipt.paired_runtime) 'Restore removed the inactive paired runtime.'
    Assert-That (Test-Path -LiteralPath $restored.invalidated_stop_proof) 'Restore did not archive the stale stop proof.'
    Assert-That ((Sha256 $registry) -eq $registryHash) 'The original native registry changed.'
    Assert-That ((Normal-RuntimeFingerprint $runtime) -eq $runtimeFingerprint) 'Restore modified the original runtime.'
    $complete = $true
    Write-Output 'Paired legacy launcher update rehearsal passed: preview, paired cutover runtime, snapshot, and restore used synthetic files only.'
} catch {
    Write-Warning "Synthetic paired-update kit retained: $kit"
    throw
} finally {
    if ($serve) {
        if (-not $serve.HasExited) { $serve.Kill(); [void]$serve.WaitForExit(3000) }
        $serve.Dispose()
    }
    if ($complete -and (Test-Path -LiteralPath $kit)) {
        $resolvedKit = (Resolve-Path -LiteralPath $kit).ProviderPath
        if ($resolvedKit.StartsWith($temp, [StringComparison]::OrdinalIgnoreCase) -and
            (Split-Path -Leaf $resolvedKit) -match '^ahs-paired-update-[0-9a-f]{32}$') {
            Remove-Item -LiteralPath $resolvedKit -Recurse -Force
        }
    }
}
