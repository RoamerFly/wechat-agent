param(
    [Parameter(Mandatory = $true)]
    [string]$NativeExe
)

# A disposable rehearsal for wx cutover-hold.  It creates one empty managed
# account through a loopback-only wx server, then stops that server before
# testing the maintenance command.  No real data root, credential, port, or
# executable is reused.
$ErrorActionPreference = 'Stop'

function Assert-That([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Write-Utf8([string]$Path, [string]$Text) {
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Free-LoopbackPort() {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return ([Net.IPEndPoint]$listener.LocalEndpoint).Port }
    finally { $listener.Stop() }
}

function Start-Native([string[]]$Arguments) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $script:native
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = [Text.UTF8Encoding]::new($false)
    $info.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    # ProcessStartInfo.ArgumentList prevents cmd.exe parsing and preserves
    # arbitrary absolute temporary paths verbatim.
    foreach ($argument in $Arguments) { [void]$info.ArgumentList.Add($argument) }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $info
    Assert-That $process.Start() 'Could not start the supplied wx.exe.'
    return $process
}

function Read-FirstJsonLine([Diagnostics.Process]$Process, [string]$Description) {
    $lineTask = $Process.StandardOutput.ReadLineAsync()
    $winner = [Threading.Tasks.Task]::WhenAny($lineTask, [Threading.Tasks.Task]::Delay(15000)).GetAwaiter().GetResult()
    if ($winner -ne $lineTask) { throw "$Description did not write its ready JSON within 15 seconds." }
    $line = $lineTask.GetAwaiter().GetResult()
    if ([string]::IsNullOrWhiteSpace($line)) {
        $stderr = $Process.StandardError.ReadToEnd()
        throw "$Description closed stdout before ready JSON. stderr: $stderr"
    }
    try { return ($line | ConvertFrom-Json) }
    catch { throw "$Description wrote non-JSON readiness output: $line" }
}

function Wait-Exit([Diagnostics.Process]$Process, [int]$Milliseconds, [string]$Description) {
    if (-not $Process.WaitForExit($Milliseconds)) { throw "$Description did not exit within $Milliseconds ms." }
}

function Get-TreeDigest([string]$Root) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $builder = [Text.StringBuilder]::new()
        foreach ($file in @(Get-ChildItem -LiteralPath $Root -Force -File -Recurse | Where-Object Name -ne 'owner.lock' | Sort-Object FullName)) {
            $relative = $file.FullName.Substring($Root.Length).TrimStart('\').Replace('\', '/')
            [void]$builder.Append($relative).Append(':').Append((Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()).Append("`n")
        }
        return ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($builder.ToString())))).Replace('-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}

function Stop-Serve([Diagnostics.Process]$Process, [string]$BaseUrl, [string]$Token, [object]$Status) {
    try {
        $body = @{ pid = [int]$Status.pid; serverId = [string]$Status.serverId; ownershipGeneration = [string]$Status.ownershipGeneration } | ConvertTo-Json -Compress
        $response = Invoke-WebRequest -UseBasicParsing -Method Post -Uri ($BaseUrl + '/v1/daemon/shutdown') -Headers @{ Authorization = 'Bearer ' + $Token } -ContentType 'application/json' -Body $body -TimeoutSec 5
        Assert-That ($response.StatusCode -eq 200) 'Synthetic wx serve rejected its bound shutdown request.'
    } finally {
        Wait-Exit $Process 15000 'Synthetic wx serve'
        Assert-That ($Process.ExitCode -eq 0) ('Synthetic wx serve exited with ' + $Process.ExitCode + '.')
        $Process.Dispose()
    }
}

function Assert-HoldReady([Diagnostics.Process]$Process, [hashtable]$Expected = @{ pendingText = 0; pendingMedia = 0; uncertainText = 0; uncertainMedia = 0 }) {
    $state = Read-FirstJsonLine $Process 'cutover-hold'
    Assert-That ($state.state -eq 'ready') 'cutover-hold did not report state=ready.'
    Assert-That ($state.mode -eq 'cutover_hold') 'cutover-hold did not report mode=cutover_hold.'
    Assert-That ([int]$state.pid -eq $Process.Id) 'cutover-hold readiness PID does not match its child process.'
    Assert-That (-not [string]::IsNullOrWhiteSpace([string]$state.serverId)) 'cutover-hold did not report a serverId.'
    Assert-That ([int]$state.accountCount -eq 1) 'cutover-hold did not report exactly one managed synthetic account.'
    foreach ($field in @('pendingText', 'pendingMedia', 'uncertainText', 'uncertainMedia')) {
        Assert-That ([int]$state.$field -eq [int]$Expected[$field]) ('cutover-hold reported an unexpected count for ' + $field + '.')
    }
    return $state
}

function Seed-SyntheticControl([string]$Database) {
    $python = (Get-Command python -ErrorAction Stop).Source
    $program = 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); now="2026-11-07T00:00:00Z"; c.executemany("INSERT INTO outbox(id,idempotency_key,payload_hash,destination,payload,state,client_id,server_id,error_code,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)", [("outbox-queued","key-queued","hash-queued","synthetic",b"x","queued","synthetic",None,None,now),("outbox-submitting","key-submitting","hash-submitting","synthetic",b"x","submitting","synthetic",None,None,now)]); c.executemany("INSERT INTO media_jobs(id,idempotency_key,request_hash,kind,state,phase,attempts,cancel_requested,retryable,error_code,data_json,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", [("media-queued","media-key-queued","media-hash-queued","send","queued","queued",0,0,0,None,"{}",b"x",now,now),("media-review","media-key-review","media-hash-review","send","needs_review","review",0,0,0,None,"{}",b"x",now,now),("media-download","media-key-download","media-hash-download","download","available","done",0,0,0,None,"{}",b"x",now,now)]); c.commit(); c.close()'
    & $python -c $program $Database
    if ($LASTEXITCODE -ne 0) { throw 'Synthetic SQLite control setup failed.' }
}

function Stop-Hold([Diagnostics.Process]$Process, [string]$Description) {
    $Process.StandardInput.WriteLine('stop')
    $Process.StandardInput.Flush()
    $Process.StandardInput.Close()
    Wait-Exit $Process 15000 $Description
    $stderr = $Process.StandardError.ReadToEnd()
    Assert-That ($Process.ExitCode -eq 0) "$Description exited $($Process.ExitCode): $stderr"
    $Process.Dispose()
}

function Assert-HoldRejected([string[]]$Arguments, [string]$Description) {
    $process = Start-Native $Arguments
    try {
        $process.StandardInput.Close()
        Wait-Exit $process 15000 $Description
        $stdout = $process.StandardOutput.ReadToEnd()
        $stderr = $process.StandardError.ReadToEnd()
        Assert-That ($process.ExitCode -ne 0) "$Description unexpectedly exited successfully. stderr: $stderr"
        Assert-That ($stdout -notmatch '"state"\s*:\s*"ready"') "$Description was admitted before its nonzero exit. stdout: $stdout stderr: $stderr"
    } finally { $process.Dispose() }
}

if (-not [IO.Path]::IsPathFullyQualified($NativeExe)) { throw '-NativeExe must be an absolute wx.exe path.' }
$native = (Resolve-Path -LiteralPath $NativeExe -ErrorAction Stop).ProviderPath
if ([IO.Path]::GetFileName($native) -ine 'wx.exe' -or -not (Test-Path -LiteralPath $native -PathType Leaf)) {
    throw '-NativeExe must name an existing wx.exe file.'
}

$testBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$kit = Join-Path $testBase ('ahs-native-cutover-hold-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $kit | Out-Null
$kitRoot = (Resolve-Path -LiteralPath $kit).ProviderPath
$complete = $false
$serve = $null
$hold = $null

try {
    # Do not pre-create data: Store::open must own and ACL its new root itself.
    $data = Join-Path $kitRoot 'data'
    $config = Join-Path $kitRoot 'native-server.toml'
    Write-Utf8 $config "schemaVersion = 1`n"
    $port = Free-LoopbackPort
    $baseUrl = 'http://127.0.0.1:' + $port

    # This server only initializes the empty synthetic store and receives a
    # single local account-create request.  It is always shut down before the
    # cutover-hold tests, so maintenance itself has neither TCP nor a token.
    $serve = Start-Native @('--json', '--server-config', $config, '--data-dir', $data, '--listen', ('127.0.0.1:' + $port), 'serve')
    $tokenPath = Join-Path $data 'api-token'
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while (-not (Test-Path -LiteralPath $tokenPath -PathType Leaf)) {
        if ($serve.HasExited -or [DateTime]::UtcNow -gt $deadline) { throw 'Synthetic wx serve did not create its temporary API token.' }
        Start-Sleep -Milliseconds 100
    }
    $token = [IO.File]::ReadAllText($tokenPath, [Text.UTF8Encoding]::new($false)).Trim()
    Assert-That (-not [string]::IsNullOrWhiteSpace($token)) 'Synthetic wx serve created an empty API token.'
    $headers = @{ Authorization = 'Bearer ' + $token }
    $status = $null
    while ($null -eq $status) {
        try { $status = Invoke-RestMethod -Method Get -Uri ($baseUrl + '/v1/status') -Headers $headers -TimeoutSec 2 }
        catch {
            if ($serve.HasExited -or [DateTime]::UtcNow -gt $deadline) { throw 'Synthetic wx serve did not become ready.' }
            Start-Sleep -Milliseconds 150
        }
    }
    $account = 'synthetic'
    $create = @{ id = $account; alias = $account } | ConvertTo-Json -Compress
    $created = Invoke-WebRequest -UseBasicParsing -Method Post -Uri ($baseUrl + '/v1/accounts') -Headers $headers -ContentType 'application/json' -Body $create -TimeoutSec 5
    Assert-That ($created.StatusCode -in 200, 201, 204) 'Synthetic account setup was rejected by local wx serve.'
    # The managed account's control store is initialized only by the normal
    # account operation path, not by hand-crafted schema.  A missing operation
    # is intentionally harmless and should be a verified 404.
    $initialized = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method Get -Uri ($baseUrl + '/v1/accounts/' + $account + '/operations/nonexistent') -Headers $headers -TimeoutSec 5
    Assert-That ($initialized.StatusCode -eq 404) 'Synthetic control-store initialization did not return the expected missing-operation response.'
    Stop-Serve $serve $baseUrl $token $status
    $serve = $null
    $control = Join-Path $data 'accounts\synthetic\agent\control.sqlite'
    Assert-That (Test-Path -LiteralPath $control -PathType Leaf) 'Synthetic account creation did not create its managed control database.'

    # A normal daemon acquires the same owner lease.  Prove the maintenance
    # command refuses it before emitting ready JSON; this is separate from a
    # second maintenance holder and exercises compatibility with wx serve.
    $leasePort = Free-LoopbackPort
    $leaseUrl = 'http://127.0.0.1:' + $leasePort
    $serve = Start-Native @('--json', '--server-config', $config, '--data-dir', $data, '--listen', ('127.0.0.1:' + $leasePort), 'serve')
    $leaseStatus = $null
    $leaseDeadline = [DateTime]::UtcNow.AddSeconds(15)
    while ($null -eq $leaseStatus) {
        try { $leaseStatus = Invoke-RestMethod -Method Get -Uri ($leaseUrl + '/v1/status') -Headers $headers -TimeoutSec 2 }
        catch {
            if ($serve.HasExited -or [DateTime]::UtcNow -gt $leaseDeadline) { throw 'Second synthetic wx serve did not become ready.' }
            Start-Sleep -Milliseconds 150
        }
    }
    Assert-HoldRejected @('--data-dir', $data, '--account', $account, 'cutover-hold') 'cutover-hold while normal wx serve owns the store'
    Stop-Serve $serve $leaseUrl $token $leaseStatus
    $serve = $null
    $before = Get-TreeDigest $data

    Assert-HoldRejected @('--data-dir', $data, '--account', 'wrong-account', 'cutover-hold') 'wrong-account cutover-hold'

    $hold = Start-Native @('--data-dir', $data, '--account', $account, 'cutover-hold')
    $null = Assert-HoldReady $hold
    Assert-That ((Get-TreeDigest $data) -eq $before) 'cutover-hold wrote to the synthetic data root after announcing ready.'
    Assert-HoldRejected @('--data-dir', $data, '--account', $account, 'cutover-hold') 'second simultaneous cutover-hold'
    Stop-Hold $hold 'first cutover-hold'
    $hold = $null
    Assert-That ((Get-TreeDigest $data) -eq $before) 'A normal cutover-hold stop changed the synthetic data root.'

    $eofHold = Start-Native @('--data-dir', $data, '--account', $account, 'cutover-hold')
    $null = Assert-HoldReady $eofHold
    $eofHold.StandardInput.Close()
    Wait-Exit $eofHold 15000 'EOF cutover-hold'
    Assert-That ($eofHold.ExitCode -ne 0) 'cutover-hold treated parent EOF as a clean stop.'
    $eofHold.Dispose()
    Assert-That ((Get-TreeDigest $data) -eq $before) 'EOF cutover-hold changed the synthetic data root.'

    # Seed representative pending/uncertain work only after the empty-store
    # pass.  The maintenance process must report it and leave the database
    # byte-for-byte untouched rather than resuming, cancelling, or migrating.
    Seed-SyntheticControl $control
    $withWork = Get-TreeDigest $data
    $hold = Start-Native @('--data-dir', $data, '--account', $account, 'cutover-hold')
    $null = Assert-HoldReady $hold @{ pendingText = 1; pendingMedia = 1; uncertainText = 1; uncertainMedia = 1 }
    Stop-Hold $hold 'reacquired cutover-hold with pending work'
    $hold = $null
    Assert-That ((Get-TreeDigest $data) -eq $withWork) 'Reacquired cutover-hold changed the queued synthetic data root.'

    # A SQLite sidecar represents an unverified in-progress writer state.
    # Leave this deliberately bad file in the disposable kit after rejection;
    # the hold command must fail closed before emitting its ready record.
    $fakeWal = Join-Path $data 'registry.sqlite-wal'
    Write-Utf8 $fakeWal 'synthetic unverified WAL sidecar'
    Assert-HoldRejected @('--data-dir', $data, '--account', $account, 'cutover-hold') 'cutover-hold with an unverified SQLite WAL sidecar'
    Assert-That (Test-Path -LiteralPath $fakeWal -PathType Leaf) 'The rejected cutover-hold unexpectedly removed the synthetic WAL sidecar.'
    $complete = $true
    Write-Output 'Native wx cutover-hold rehearsal passed: empty synthetic account, hold protocol, rejection, EOF, and reacquisition.'
} catch {
    Write-Warning "Synthetic native hold directory retained for inspection: $kitRoot"
    throw
} finally {
    if ($hold -and -not $hold.HasExited) { try { $hold.StandardInput.WriteLine('stop'); $hold.StandardInput.Close(); $hold.WaitForExit(3000) | Out-Null } catch { } }
    if ($hold) { $hold.Dispose() }
    if ($serve -and -not $serve.HasExited) { try { $serve.Kill(); $serve.WaitForExit(3000) | Out-Null } catch { } }
    if ($serve) { $serve.Dispose() }
    if ($complete -and (Test-Path -LiteralPath $kitRoot)) {
        $resolvedKit = (Resolve-Path -LiteralPath $kitRoot).ProviderPath
        if ($resolvedKit.StartsWith($testBase, [StringComparison]::OrdinalIgnoreCase) -and
            (Split-Path -Leaf $resolvedKit) -match '^ahs-native-cutover-hold-[0-9a-f]{32}$') {
            Remove-Item -LiteralPath $resolvedKit -Recurse -Force
        }
    }
}
