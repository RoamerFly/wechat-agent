param([string]$OutputDirectory,[string]$SourceDirectory,[switch]$Test,[string]$GoSourcePatch)
$ErrorActionPreference='Stop'
$workspace=Split-Path $PSScriptRoot -Parent

if(-not $OutputDirectory){$OutputDirectory=Join-Path $workspace 'dist\windows-native\runtimes\windows-x64'}
if(-not $SourceDirectory){$SourceDirectory=Join-Path $workspace 'temp\windows-native-build'}
$SourceDirectory=[IO.Path]::GetFullPath($SourceDirectory)
$OutputDirectory=[IO.Path]::GetFullPath($OutputDirectory)
# MSVC/OpenSSL still has build steps sensitive to MAX_PATH. Keep generated
# paths short even when the upstream checkout has several nested directories.
$nativeTarget=Join-Path $workspace 'temp\wn-target'
$manifest=Get-Content -LiteralPath (Join-Path $workspace 'components\native-client\source-manifest.json') -Raw | ConvertFrom-Json
$patch=Join-Path $workspace $manifest.patch
if((Get-FileHash -LiteralPath $patch -Algorithm SHA256).Hash -ne $manifest.patch_sha256){throw 'Native integration patch checksum mismatch.'}
if(-not(Test-Path -LiteralPath $SourceDirectory)){
 & git clone --filter=blob:none --no-checkout $manifest.source_url $SourceDirectory
 if($LASTEXITCODE -ne 0){throw 'Native source download failed.'}
 & git -C $SourceDirectory sparse-checkout set packages/native-client-rust
 if($LASTEXITCODE -ne 0){throw 'Native sparse checkout failed.'}
 & git -C $SourceDirectory checkout --detach $manifest.base_commit
 if($LASTEXITCODE -ne 0){throw 'Native source checkout failed.'}
}
$revision=& git -C $SourceDirectory rev-parse HEAD
if($LASTEXITCODE -ne 0 -or $revision -ne $manifest.base_commit){throw 'Native source must match the fixed project commit.'}
# Reuse the same cumulative patch on Windows. Never stack it twice.
$oldPreference=$ErrorActionPreference
try {
 # An unapplied patch is the expected result on a new checkout. Windows
 # PowerShell 5 treats redirected native stderr as a terminating error when
 # ErrorActionPreference=Stop; inspect only this probe's exit status instead.
 $ErrorActionPreference='Continue'
 & git -C $SourceDirectory apply --reverse --check $patch 2>$null
 $patchApplied=$LASTEXITCODE -eq 0
} finally {$ErrorActionPreference=$oldPreference}
if(-not $patchApplied){
 & git -C $SourceDirectory apply --check $patch
 if($LASTEXITCODE -ne 0){throw 'Native integration patch does not match this source; no reset performed.'}
 & git -C $SourceDirectory apply $patch
 if($LASTEXITCODE -ne 0){throw 'Native integration patch failed.'}
}
# Reject extra edits: the build must contain exactly the recorded tracked patch
# and its explicitly listed new files. No local source is reset or discarded.
$actual=& git -C $SourceDirectory status --porcelain --untracked-files=all
$allowed=@()
foreach($line in Get-Content -LiteralPath $patch){if($line -match '^\+\+\+ b/(.+)$'){$allowed+=$matches[1]}}
foreach($line in $actual){if($line.Length -gt 3 -and $allowed -notcontains $line.Substring(3).Replace('\','/')){throw 'Unrelated native source changes found; build refused.'}}
$oldIndex=$env:GIT_INDEX_FILE
$validationIndex=Join-Path $workspace ('temp\native-validation-index-'+[Guid]::NewGuid().ToString('N'))
try {
 $env:GIT_INDEX_FILE=$validationIndex
 & git -C $SourceDirectory read-tree HEAD
 if($LASTEXITCODE -ne 0){throw 'Cannot create isolated source validation index.'}
 & git -C $SourceDirectory add -A -- packages/native-client-rust
 if($LASTEXITCODE -ne 0){throw 'Cannot inspect native source.'}
 & git -C $SourceDirectory apply --reverse --cached $patch
 if($LASTEXITCODE -ne 0){throw 'Native source differs from the recorded patch.'}
 & git -C $SourceDirectory diff --cached --quiet HEAD -- packages/native-client-rust
 if($LASTEXITCODE -ne 0){throw 'Additional native source edits detected; build refused.'}
} finally {
 $env:GIT_INDEX_FILE=$oldIndex
 if(Test-Path -LiteralPath $validationIndex){Remove-Item -LiteralPath $validationIndex}
}
$vswhere=Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
if(Test-Path -LiteralPath $vswhere){
 $vs=& $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
 if($vs){
  $dev=Join-Path $vs 'Common7\Tools\VsDevCmd.bat'
  $devOutput=& $env:ComSpec /d /c ('"'+$dev+'" -arch=amd64 -host_arch=amd64 >nul && set')
  if($LASTEXITCODE -ne 0){throw 'MSVC developer environment failed.'}
  foreach($entry in $devOutput){if($entry -match '^([^=]+)=(.*)$'){[Environment]::SetEnvironmentVariable($matches[1],$matches[2],'Process')}}
 }
}
# Git's minimal MSYS Perl can lack required modules and is not a substitute
# for a complete native Windows Perl. Honor an explicit PERL build setting.
Push-Location (Join-Path $SourceDirectory 'packages\native-client-rust')
$oldTarget=$env:CARGO_TARGET_DIR
try {
 $env:CARGO_TARGET_DIR=$nativeTarget
 & cargo build --release --locked -p wx-native --bin wx --target x86_64-pc-windows-msvc
 if($LASTEXITCODE -ne 0){throw 'Native Windows compilation failed. Rust, MSVC, Perl and Windows SDK are build-time prerequisites.'}
 if($Test){
  & cargo test --release --locked --target x86_64-pc-windows-msvc -p wx-native -p wechat-profile --test initialize --test login_storage --test media_write --test mmkv_write --test profile --test projection
  if($LASTEXITCODE -ne 0){throw 'Windows profile initialization, credential, media and projection tests failed.'}
  & cargo test --release --locked --target x86_64-pc-windows-msvc -p wx-native -p wechat-store --test windows_bootstrap
  if($LASTEXITCODE -ne 0){throw 'Windows interrupted profile recovery test failed.'}
 }
} finally {$env:CARGO_TARGET_DIR=$oldTarget; Pop-Location}
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
Copy-Item -LiteralPath (Join-Path $nativeTarget 'x86_64-pc-windows-msvc\release\wx.exe') -Destination (Join-Path $OutputDirectory 'wx.exe')
# App-local redistributable DLLs avoid a separate machine-wide VC install.
# These are build-tool redistributables, never downloaded binaries in Git.
if($env:VCToolsRedistDir){
 $crt=Get-ChildItem -LiteralPath (Join-Path $env:VCToolsRedistDir 'x64') -Directory | Where-Object Name -like 'Microsoft.VC*.CRT' | Select-Object -Last 1
 if($crt){Get-ChildItem -LiteralPath $crt.FullName -Filter '*.dll' -File | ForEach-Object {Copy-Item -LiteralPath $_.FullName -Destination $OutputDirectory}}
}
if(-not(Test-Path -LiteralPath (Join-Path $OutputDirectory 'vcruntime140.dll'))){throw 'The app-local Microsoft VC release redistributable is required for the portable build.'}
Copy-Item -LiteralPath (Join-Path $workspace 'components\native-client\MSVC-REDISTRIBUTABLE-NOTICE.txt') -Destination $OutputDirectory
$oldOS=$env:GOOS;$oldArch=$env:GOARCH;$oldCGO=$env:CGO_ENABLED
try {
 $env:GOOS='windows';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
 # Build Go from a committed source snapshot, excluding unrelated local edits.
 $goRevision=& git -C $workspace rev-parse HEAD
 if($LASTEXITCODE -ne 0){throw 'Cannot identify committed management source.'}
 $goSource=Join-Path $workspace ('temp\win-go-'+[Guid]::NewGuid().ToString('N').Substring(0,8))
 New-Item -ItemType Directory -Path $goSource | Out-Null
 $goArchive=Join-Path $goSource 'source.tar'
 & git -C $workspace archive --format=tar ('--output='+$goArchive) HEAD cmd connected-apps internal plugins go.mod go.sum
 if($LASTEXITCODE -ne 0){throw 'Cannot export committed management source.'}
 & tar -xf $goArchive -C $goSource
 if($LASTEXITCODE -ne 0){throw 'Cannot prepare committed management source.'}
 $goPatchHash=$null
 if($GoSourcePatch){
  $GoSourcePatch=[IO.Path]::GetFullPath($GoSourcePatch)
  & git -C $goSource init -q
  if($LASTEXITCODE -ne 0){throw 'Cannot isolate local patch application from the parent checkout.'}
  foreach($line in Get-Content -LiteralPath $GoSourcePatch){
   if($line -match '^\+\+\+ b/(.+)$' -and $matches[1] -notmatch '^(internal/|cmd/|plugins/|go\.mod$|go\.sum$)'){throw 'Local source patch contains a path outside management source.'}
  }
  & git -C $goSource apply --check $GoSourcePatch
  if($LASTEXITCODE -ne 0){throw 'Local management source patch failed validation.'}
  & git -C $goSource apply $GoSourcePatch
  if($LASTEXITCODE -ne 0){throw 'Cannot apply local management source patch.'}
  $goPatchHash=(Get-FileHash -LiteralPath $GoSourcePatch -Algorithm SHA256).Hash.ToLower()
 }
 Push-Location $goSource
 try {
  $dependencies=@(& go list -deps ./cmd/wechat-admin)
  if($LASTEXITCODE -ne 0){throw 'Windows management server dependency check failed.'}
  if($dependencies -match '^wechat-bot/internal/(adbserver|devicebroker)$'){throw 'Windows management server includes retired device execution dependencies.'}
  & go build -buildvcs=false -trimpath -o (Join-Path $OutputDirectory 'wechat-admin.exe') ./cmd/wechat-admin
  if($LASTEXITCODE -ne 0){throw 'Windows management server build failed.'}
 } finally {Pop-Location}
} finally {$env:GOOS=$oldOS;$env:GOARCH=$oldArch;$env:CGO_ENABLED=$oldCGO}
$license=Join-Path $workspace 'components\native-client\LICENSE.upstream'
if((Get-FileHash -LiteralPath $license -Algorithm SHA256).Hash -ne 'D4D971ACB3CAC94865AAD4787AB934038DF90A304854B41F506AE1187350B574'){throw 'Upstream license checksum mismatch.'}
Copy-Item -LiteralPath $license -Destination (Join-Path $OutputDirectory 'agent-wechat-LICENSE.txt')
$notices=@{'packages/native-client-rust/crates/wechat-media/NOTICE.md'='wechat-media-NOTICE.txt';'packages/native-client-rust/crates/wechat-media/LICENSE-CHATLOG'='wechat-media-LICENSE-CHATLOG.txt';'packages/native-client-rust/crates/wechat-protocol/NOTICE'='wechat-protocol-NOTICE.txt';'packages/native-client-rust/crates/wechat-protocol/vendor/silk/LICENSE-MIT'='silk-LICENSE-MIT.txt';'packages/native-client-rust/crates/wechat-protocol/vendor/silk/LICENSE-SDK'='silk-LICENSE-SDK.txt'}
foreach($entry in $notices.GetEnumerator()){Copy-Item -LiteralPath (Join-Path $SourceDirectory $entry.Key) -Destination (Join-Path $OutputDirectory $entry.Value)}
$record=[ordered]@{schema_version=1;platform='windows-x64';native_source=$manifest.source_url;native_commit=$manifest.base_commit;native_patch_sha256=$manifest.patch_sha256;native_license_commit='7527cfc455be74b258cba34f75f38523b59ba1c7';native_sha256=(Get-FileHash -LiteralPath (Join-Path $OutputDirectory 'wx.exe') -Algorithm SHA256).Hash.ToLower();admin_source_commit=$goRevision;admin_sha256=(Get-FileHash -LiteralPath (Join-Path $OutputDirectory 'wechat-admin.exe') -Algorithm SHA256).Hash.ToLower();live_login_verified=$false;live_delivery_verified=$false}
if($goPatchHash){$record['admin_source_patch_sha256']=$goPatchHash}
[IO.File]::WriteAllText((Join-Path $OutputDirectory 'windows-native-manifest.json'),($record | ConvertTo-Json),[Text.UTF8Encoding]::new($false))
Write-Output 'Windows native runtime built; private data and models were not packaged.'
