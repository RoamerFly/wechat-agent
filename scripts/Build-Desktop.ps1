param([string]$OutputDirectory, [switch]$Test, [switch]$Portable, [switch]$LocalStation, [switch]$WithWeChatApp, [switch]$WithWeChatRuntime, [switch]$WithShareReader, [switch]$WithMediaUnderstanding, [switch]$WithDocumentWorkshop, [switch]$WithAudioWorkshop, [switch]$WithRVCBackend, [switch]$WithComfyUI, [switch]$WithYuE, [switch]$WithStationSpeech, [switch]$WithOBSPreview, [string]$WeChatRuntimeDirectory, [string]$NativeSourceDirectory, [string]$MediaDirectory, [string]$GoSourcePatch, [string]$DocumentPythonPackagesDirectory, [string]$DocumentTesseractDirectory, [string]$DocumentFontsDirectory, [string]$DocumentPDFiumWheel, [switch]$WithBilibiliApp, [switch]$WithDouyinApp)
$ErrorActionPreference='Stop'
$workspace=Split-Path $PSScriptRoot -Parent

function Get-RelativeRuntimePath([string]$Root,[string]$Path){
 $rootPath=[IO.Path]::GetFullPath($Root).TrimEnd('\','/')
 $fullPath=[IO.Path]::GetFullPath($Path)
 if(-not $fullPath.StartsWith($rootPath+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)){throw 'Runtime source file escaped the selected directory.'}
 return $fullPath.Substring($rootPath.Length+1).Replace('\','/')
}

function Assert-NoReparsePointAncestor([string]$Path){
 $candidate=[IO.Path]::GetFullPath($Path)
 while(-not(Test-Path -LiteralPath $candidate)){
  $parent=Split-Path -Path $candidate -Parent
  if(-not $parent -or $parent -eq $candidate){throw 'Cannot locate a physical parent directory for the runtime output.'}
  $candidate=$parent
 }
 while($candidate){
  $item=Get-Item -LiteralPath $candidate -Force
  if($item.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'The runtime output path contains a reparse-point ancestor and cannot be used.'}
  $parent=Split-Path -Path $candidate -Parent
  if(-not $parent -or $parent -eq $candidate){break}
  $candidate=$parent
 }
}

function Test-LocalStationRuntimeIdentical([string]$Existing,[System.Collections.IDictionary]$ExpectedFiles,[string]$ExpectedNativeHash){
 if(-not(Test-Path -LiteralPath $Existing -PathType Container)){return $false}
 Assert-NoReparsePointAncestor $Existing
 $manifestPath=Join-Path $Existing 'windows-native-manifest.json'
 if(-not(Test-Path -LiteralPath $manifestPath -PathType Leaf)){return $false}
 Assert-NoReparsePointAncestor $manifestPath
 try {$manifest=Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json} catch {return $false}
 if($manifest.schema_version -ne 1 -or $manifest.platform -ne 'windows-x64' -or -not $manifest.local_station_runtime -or $manifest.native_sha256 -ne $ExpectedNativeHash){return $false}
 $actual=@{}
 foreach($file in @(Get-ChildItem -LiteralPath $Existing -Force)){
  if($file.PSIsContainer -or ($file.Attributes -band [IO.FileAttributes]::ReparsePoint)){return $false}
  $relative=Get-RelativeRuntimePath $Existing $file.FullName
  if($relative -eq 'windows-native-manifest.json'){continue}
  $actual[$relative]=(Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLower()
 }
 if($actual.Count -ne $ExpectedFiles.Count){return $false}
 foreach($entry in $ExpectedFiles.GetEnumerator()){
  if(-not $actual.ContainsKey($entry.Key) -or $actual[$entry.Key] -ne $entry.Value){return $false}
  $manifestFile=$manifest.files.PSObject.Properties[$entry.Key]
  if($null -eq $manifestFile -or $manifestFile.Value -ne $entry.Value){return $false}
 }
 return $true
}

function Stage-LocalStationWeChatRuntime([string]$SourceDirectory,[string]$PackageDirectory){
 if(-not $SourceDirectory){throw 'WithWeChatRuntime requires -WeChatRuntimeDirectory. Select only a previously verified windows-x64 runtime directory.'}
 $source=[IO.Path]::GetFullPath($SourceDirectory)
 if(-not(Test-Path -LiteralPath $source -PathType Container)){throw 'The selected WeChat runtime directory does not exist.'}
 Assert-NoReparsePointAncestor $source
 $entries=Get-ChildItem -LiteralPath $source -Force
 if($entries | Where-Object {$_.Attributes -band [IO.FileAttributes]::ReparsePoint}){throw 'The selected WeChat runtime contains a reparse point and cannot be staged.'}
 # A historical Windows release can contain the separate punctuation Python
 # environment.  That is an optional processing tool, not a wx.exe runtime;
 # copying it would silently pull an unrelated model and Python packages into
 # the lightweight station.  Stage only the native-client root files.
 if($entries | Where-Object {$_.PSIsContainer -and $_.Name -ne 'punctuation'}){throw 'The selected WeChat runtime contains an unexpected subdirectory.'}
 $files=@($entries | Where-Object {-not $_.PSIsContainer})
 $manifestPath=Join-Path $source 'windows-native-manifest.json'
 if(-not(Test-Path -LiteralPath $manifestPath -PathType Leaf)){throw 'The selected WeChat runtime has no windows-native-manifest.json.'}
 try {$sourceManifest=Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json} catch {throw 'The selected WeChat runtime manifest is not valid JSON.'}
 if($sourceManifest.schema_version -ne 1 -or $sourceManifest.platform -ne 'windows-x64'){throw 'The selected WeChat runtime manifest is not a Windows x64 native bundle.'}
 $wxSource=Join-Path $source 'wx.exe'
 if(-not(Test-Path -LiteralPath $wxSource -PathType Leaf)){throw 'The selected WeChat runtime has no wx.exe.'}
 $wxHash=(Get-FileHash -LiteralPath $wxSource -Algorithm SHA256).Hash.ToLower()
 if([string]::IsNullOrWhiteSpace($sourceManifest.native_sha256) -or $sourceManifest.native_sha256.ToLower() -ne $wxHash){throw 'The selected WeChat runtime wx.exe does not match its manifest hash.'}

 $required=@('wx.exe','vcruntime140.dll','MSVC-REDISTRIBUTABLE-NOTICE.txt','agent-wechat-LICENSE.txt')
 $sourceNames=@($files | ForEach-Object {Get-RelativeRuntimePath $source $_.FullName})
 foreach($name in $required){if($sourceNames -notcontains $name){throw ('The selected WeChat runtime is incomplete: missing '+$name+'.')}}
 foreach($file in $files){
  $relative=Get-RelativeRuntimePath $source $file.FullName
  # The legacy native package includes the old management server.  It is not
  # a LocalStation dependency and must never be copied into this bundle.
  if($relative -in @('windows-native-manifest.json','wechat-admin.exe')){continue}
  $allowed=($relative -eq 'wx.exe') -or ($relative -match '(?i)\.dll$') -or ($relative -match '(?i)(license|notice).*\.txt$')
  if(-not $allowed){throw ('The selected WeChat runtime contains a file that is not a native dependency or notice: '+$relative)}
 }

 $runtimeRoot=Join-Path $PackageDirectory 'runtimes'
 $destination=Join-Path $runtimeRoot 'windows-x64'
 Assert-NoReparsePointAncestor $PackageDirectory
 New-Item -ItemType Directory -Force -Path $runtimeRoot | Out-Null
 Assert-NoReparsePointAncestor $runtimeRoot
 $staging=$destination+'.staging-'+[Guid]::NewGuid().ToString('N')
 New-Item -ItemType Directory -Force -Path $staging | Out-Null
 $stagingCreated=$true
 try {
  $hashes=[ordered]@{}
  foreach($file in $files){
   $relative=Get-RelativeRuntimePath $source $file.FullName
   if($relative -in @('windows-native-manifest.json','wechat-admin.exe')){continue}
   $target=Join-Path $staging $relative.Replace('/','\')
   New-Item -ItemType Directory -Force -Path (Split-Path $target -Parent) | Out-Null
   Copy-Item -LiteralPath $file.FullName -Destination $target
   $sourceHash=(Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLower()
   $targetHash=(Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLower()
   if($sourceHash -ne $targetHash){throw ('WeChat runtime copy hash mismatch: '+$relative)}
   $hashes[$relative]=$targetHash
  }
  if($hashes['wx.exe'] -ne $wxHash){throw 'The staged wx.exe hash is not the validated native hash.'}
  $record=[ordered]@{
   schema_version=1
   platform='windows-x64'
   local_station_runtime=$true
   native_source=$sourceManifest.native_source
   native_commit=$sourceManifest.native_commit
   native_patch_sha256=$sourceManifest.native_patch_sha256
   native_license_commit=$sourceManifest.native_license_commit
   native_sha256=$wxHash
   files=$hashes
   live_login_verified=$false
   live_delivery_verified=$false
  }
  [IO.File]::WriteAllText((Join-Path $staging 'windows-native-manifest.json'),($record | ConvertTo-Json -Depth 6),[Text.UTF8Encoding]::new($false))
  if((Get-FileHash -LiteralPath (Join-Path $staging 'wx.exe') -Algorithm SHA256).Hash.ToLower() -ne $wxHash){throw 'The staged wx.exe failed its final hash validation.'}
  if(Test-Path -LiteralPath $destination){
   if(Test-LocalStationRuntimeIdentical -Existing $destination -ExpectedFiles $hashes -ExpectedNativeHash $wxHash){
    Write-Output ('Verified existing WeChat native runtime: '+$destination+'; it was not replaced.')
    return
   }
   throw 'The package already contains a different WeChat runtime. Choose a fresh output directory; an existing runtime is never replaced.'
  }
  # Directory.Move refuses an already-created destination.  This prevents a
  # concurrent process from making Move-Item nest the staged directory into a
  # raced target after we performed the identical-bundle check above.
  [IO.Directory]::Move($staging,$destination)
  $stagingCreated=$false
 } finally {
  if($stagingCreated -and (Test-Path -LiteralPath $staging)){Remove-Item -LiteralPath $staging -Recurse -Force}
 }
 Write-Output ('Staged verified WeChat native runtime: '+$destination+' ('+$files.Count+' files; no account data or management executable).')
}

function Stage-LocalStationSpeechRunner([string]$PackageDirectory){
 # Keep the trusted repository runner as the only source.  This package is a
 # preview pin for the registered backend, not a bundled Python environment,
 # model, voice profile or executable adapter.
 $source=Join-Path $workspace 'plugins\speech\runtime\gpt_sovits.py'
 if(-not(Test-Path -LiteralPath $source -PathType Leaf)){throw 'The trusted local speech runner is missing from plugins/speech/runtime.'}
 Assert-NoReparsePointAncestor $source
 $hash=(Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLower()
 $runtimeRoot=Join-Path $PackageDirectory 'runtimes'
 $destination=Join-Path $runtimeRoot 'windows-station-speech'
 $manifestPath=Join-Path $destination 'runtime-manifest.json'
 Assert-NoReparsePointAncestor $PackageDirectory
 New-Item -ItemType Directory -Force -Path $runtimeRoot | Out-Null
 Assert-NoReparsePointAncestor $runtimeRoot
 $files=[ordered]@{'gpt_sovits.py'=$hash}
 $manifest=([ordered]@{schema_version=1;platform='windows-x64';policy='station-speech-runner-v1';source='plugins/speech/runtime/gpt_sovits.py';files=$files}|ConvertTo-Json -Depth 4)
 if(Test-Path -LiteralPath $destination){
  Assert-NoReparsePointAncestor $destination
  if(-not(Test-Path -LiteralPath $manifestPath -PathType Leaf) -or [IO.File]::ReadAllText($manifestPath) -cne $manifest){throw 'Existing station speech runner differs; use a fresh output directory.'}
  $entries=@(Get-ChildItem -LiteralPath $destination -Force)
  if($entries.Count -ne 2 -or $entries | Where-Object {$_.PSIsContainer -or ($_.Attributes -band [IO.FileAttributes]::ReparsePoint)}){throw 'Existing station speech runner has unexpected files.'}
  $staged=Join-Path $destination 'gpt_sovits.py'
  if(-not(Test-Path -LiteralPath $staged -PathType Leaf) -or (Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash.ToLower() -ne $hash){throw 'Existing station speech runner integrity check failed.'}
  Write-Output ('Verified existing station speech runner: '+$staged+' (SHA-256 '+$hash+').')
  return
 }
 New-Item -ItemType Directory -Path $destination | Out-Null
 $staged=Join-Path $destination 'gpt_sovits.py'
 Copy-Item -LiteralPath $source -Destination $staged
 if((Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash.ToLower() -ne $hash){throw 'Station speech runner source changed during staging.'}
 [IO.File]::WriteAllText($manifestPath,$manifest,[Text.UTF8Encoding]::new($false))
 Write-Output ('Staged trusted station speech runner: '+$staged+' (SHA-256 '+$hash+'). Backend registration must use this explicit absolute path and digest.')
}

if($WithWeChatRuntime -and -not $LocalStation){throw 'WithWeChatRuntime is only valid with -LocalStation.'}
if($WithRVCBackend -and -not $LocalStation){throw 'WithRVCBackend is only valid with -LocalStation.'}
if($WithComfyUI -and -not $LocalStation){throw 'WithComfyUI is only valid with -LocalStation.'}
if($WithYuE -and -not $LocalStation){throw 'WithYuE is only valid with -LocalStation.'}
if($WithStationSpeech -and -not $LocalStation){throw 'WithStationSpeech is only valid with -LocalStation.'}
if($WithOBSPreview -and -not $LocalStation){throw 'WithOBSPreview is only valid with -LocalStation.'}
if($WithBilibiliApp -and -not $LocalStation){throw 'WithBilibiliApp is only valid with -LocalStation.'}
if($WithDouyinApp -and -not $LocalStation){throw 'WithDouyinApp is only valid with -LocalStation.'}
if($WithWeChatRuntime){$WithWeChatApp=$true}
# A document converter is intentionally opt-in.  The three existing source
# directory arguments are the opt-in contract: when absent, a LocalStation
# document workshop remains able to create DOCX files but carries no PDF/OCR
# runtime.  Do not accept a partial set of sources.  The regular desktop
# catalog contains the supported tools, while a LocalStation catalog is opt-in;
# therefore only the latter must explicitly select document-workshop.
$hasDocumentRuntimeInput=[bool]($DocumentPythonPackagesDirectory -or $DocumentFontsDirectory -or $DocumentTesseractDirectory -or $DocumentPDFiumWheel)
if($LocalStation -and $hasDocumentRuntimeInput -and -not $WithDocumentWorkshop){throw 'Native document runtime sources require -WithDocumentWorkshop for a LocalStation package.'}
if($hasDocumentRuntimeInput -and (-not $DocumentPythonPackagesDirectory -or -not $DocumentFontsDirectory)){throw 'Native document runtime requires both DocumentPythonPackagesDirectory and DocumentFontsDirectory.'}
# Keep project-local SDK/model caches outside `go test ./...` discovery.
# Nested build snapshots retain their own go.mod; never replace an existing one.
foreach($artifactName in @('temp','dist')){
 $artifactDirectory=Join-Path $workspace $artifactName
 New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
 $boundary=Join-Path $artifactDirectory 'go.mod'
 if(-not(Test-Path -LiteralPath $boundary)){
  [IO.File]::WriteAllText($boundary,("module local.invalid/wechat-agent-"+$artifactName+"`n`ngo 1.25.3`n"),[Text.Encoding]::ASCII)
 }
}
if(-not $OutputDirectory){$OutputDirectory=Join-Path $workspace ($(if($LocalStation){'dist\local-station-preview'}else{'dist\windows-desktop'}))}
$OutputDirectory=[IO.Path]::GetFullPath($OutputDirectory)
# Rebuilding is not a migration or a request to delete previous output.
foreach($relative in @('plugins\mobile-control','runtimes\windows-music\mobile_bridge.py')){
 $obsolete=Join-Path $OutputDirectory $relative
 if(Get-Item -LiteralPath $obsolete -Force -ErrorAction SilentlyContinue){
  throw 'Output contains retired mobile components; build into a fresh directory.'
 }
}
$version='1.0.3537.50'
$expectedHash='5EA526BBD728ADDA0DA4D31219267E96460494A427E4894C4E09D9F320F4B9AA'
$cache=Join-Path $workspace ('temp\desktop-deps\webview2-'+$version)
$archive=$cache+'.zip'
New-Item -ItemType Directory -Force -Path (Split-Path $cache -Parent),$OutputDirectory | Out-Null
# The zero-application preview deliberately has no optional tool catalog.  The
# regular desktop package retains the catalog validation/staging contract.
if((-not $LocalStation) -or $WithShareReader -or $WithMediaUnderstanding -or $WithDocumentWorkshop -or $WithAudioWorkshop -or $WithRVCBackend -or $WithComfyUI -or $WithYuE -or $WithStationSpeech -or $WithOBSPreview){
 $pluginSource=Join-Path $workspace 'plugins'
 $catalogArgs=@('--source',$pluginSource,'--output',(Join-Path $OutputDirectory 'plugins'))
 if(-not $LocalStation){
  $supported=@(Get-ChildItem -LiteralPath $pluginSource -Directory | Where-Object Name -ne 'mobile-control')
  if(-not $supported.Count){throw 'No supported tools found for the Windows desktop catalog.'}
  foreach($package in $supported){$catalogArgs+=@('--package',$package.Name)}
 }
 if($LocalStation -and $WithShareReader){$catalogArgs+=@('--package','share-reader')}
 if($LocalStation -and $WithMediaUnderstanding){$catalogArgs+=@('--package','media-understanding')}
 if($LocalStation -and $WithDocumentWorkshop){$catalogArgs+=@('--package','document-workshop')}
 if($LocalStation -and $WithAudioWorkshop){$catalogArgs+=@('--package','audio-workshop')}
 if($LocalStation -and $WithRVCBackend){$catalogArgs+=@('--package','rvc')}
 if($LocalStation -and $WithComfyUI){$catalogArgs+=@('--package','comfyui')}
 if($LocalStation -and $WithYuE){$catalogArgs+=@('--package','yue')}
 if($LocalStation -and $WithStationSpeech){$catalogArgs+=@('--package','station-speech')}
 if($LocalStation -and $WithOBSPreview){$catalogArgs+=@('--package','obs-preview')}
 & python -B (Join-Path $PSScriptRoot 'stage_plugin_catalog.py') @catalogArgs
 if($LASTEXITCODE -ne 0){throw 'Official plugin catalog staging failed; use a fresh output directory.'}
}
# Adding application source does not connect an account or enable its runtime.
$appPackages=@()
if((-not $LocalStation) -or $WithWeChatApp){$appPackages+=@('--package','wechat')}
if($WithBilibiliApp){$appPackages+=@('--package','bilibili')}
if($WithDouyinApp){$appPackages+=@('--package','douyin')}
if($appPackages.Count){
 & python -B (Join-Path $PSScriptRoot 'stage_plugin_catalog.py') --source (Join-Path $workspace 'connected-apps') --output (Join-Path $OutputDirectory 'connected-apps') --descriptor app.json @appPackages
 if($LASTEXITCODE -ne 0){throw 'Application source staging failed; use a fresh output directory.'}
}
if(-not(Test-Path -LiteralPath $archive)){
 Invoke-WebRequest -Uri ('https://api.nuget.org/v3-flatcontainer/microsoft.web.webview2/'+$version+'/microsoft.web.webview2.'+$version+'.nupkg') -OutFile $archive
}
if((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $expectedHash){throw 'WebView2 SDK package hash mismatch; build refused.'}
if(-not(Test-Path -LiteralPath (Join-Path $cache 'lib\net462\Microsoft.Web.WebView2.Core.dll'))){Expand-Archive -LiteralPath $archive -DestinationPath $cache -Force}
$compiler=Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if(-not(Test-Path -LiteralPath $compiler)){throw 'The Windows .NET Framework C# compiler is required.'}
$source=Join-Path $workspace 'desktop\windows'
$core=Join-Path $cache 'lib\net462\Microsoft.Web.WebView2.Core.dll'
$forms=Join-Path $cache 'lib\net462\Microsoft.Web.WebView2.WinForms.dll'
$loader=Join-Path $cache 'runtimes\win-x64\native\WebView2Loader.dll'
$target=Join-Path $OutputDirectory ($(if($LocalStation){'AgentHomeStation.exe'}else{'WechatAgent.exe'}))
$arguments=@('/nologo','/target:winexe','/platform:x64','/optimize+','/utf8output',('/out:'+$target),('/win32manifest:'+(Join-Path $source 'app.manifest')),'/reference:System.Windows.Forms.dll','/reference:System.Drawing.dll','/reference:System.Net.Http.dll','/reference:System.Web.Extensions.dll',('/reference:'+$core),('/reference:'+$forms),('/resource:'+$core+',Microsoft.Web.WebView2.Core.dll'),('/resource:'+$forms+',Microsoft.Web.WebView2.WinForms.dll'),('/resource:'+$loader+',WebView2Loader.dll'),(Join-Path $source 'Deployment.cs'),(Join-Path $source 'WindowsRuntime.cs'),(Join-Path $source 'LocalStationRuntime.cs'),(Join-Path $source 'CloseSettings.cs'),(Join-Path $source 'CloseChoiceDialog.cs'),(Join-Path $source 'DesktopInstance.cs'),(Join-Path $source 'DouyinAuthorizationDialog.cs'),(Join-Path $source 'Program.cs'),(Join-Path $source 'MainForm.cs'))
if($LocalStation){$arguments+='/define:LOCAL_STATION'}
$icon=Join-Path $source 'assets\agent-home.ico'
$arguments+=('/resource:'+(Join-Path $source 'assets\agent-home.png')+',AgentHomeStation.Icon.png')
$arguments+=(Join-Path $source 'LifecycleSurface.cs')
if(-not(Test-Path -LiteralPath $icon)){throw 'The project desktop icon is missing.'}
$arguments+=('/win32icon:'+$icon)
& $compiler @arguments
if($LASTEXITCODE -ne 0){throw 'Desktop build failed.'}
if($LocalStation){
 $station=Join-Path $OutputDirectory 'agent-home-station-server.exe'
 $oldOS=$env:GOOS; $oldArch=$env:GOARCH
 Push-Location $workspace
 try {
  $env:GOOS='windows'; $env:GOARCH='amd64'
  $dependencies=@(& go list -deps ./cmd/agent-home-station)
  if($LASTEXITCODE -ne 0){throw 'LocalStation Windows dependency check failed.'}
  if($dependencies -match '^wechat-bot/internal/(adbserver|devicebroker)$'){throw 'LocalStation server includes retired device execution dependencies.'}
  & go build -trimpath -o $station './cmd/agent-home-station'
  if($LASTEXITCODE -ne 0){throw 'LocalStation server build failed.'}
 } finally {$env:GOOS=$oldOS; $env:GOARCH=$oldArch; Pop-Location}
 if($WithWeChatRuntime){Stage-LocalStationWeChatRuntime -SourceDirectory $WeChatRuntimeDirectory -PackageDirectory $OutputDirectory}
 if($WithStationSpeech){Stage-LocalStationSpeechRunner -PackageDirectory $OutputDirectory}
}
Copy-Item -LiteralPath (Join-Path $cache 'LICENSE.txt') -Destination (Join-Path $OutputDirectory 'WebView2-LICENSE.txt')
$gorillaLicense=Join-Path $workspace 'docs\licenses\gorilla-websocket.txt'
if(-not(Test-Path -LiteralPath $gorillaLicense -PathType Leaf)){throw 'The Gorilla WebSocket license notice is missing.'}
Copy-Item -LiteralPath $gorillaLicense -Destination (Join-Path $OutputDirectory 'Gorilla-WebSocket-LICENSE.txt')
Copy-Item -LiteralPath (Join-Path $workspace 'docs\licenses\rtmp-relay.txt') -Destination (Join-Path $OutputDirectory 'RTMP-Relay-LICENSE.txt') -ErrorAction Stop
if($Test){
 $tests=Join-Path $OutputDirectory 'DesktopTests.exe'
 & $compiler /nologo /target:exe /platform:x64 /utf8output ('/out:'+$tests) /reference:System.Net.Http.dll /reference:System.Web.Extensions.dll (Join-Path $source 'Deployment.cs') (Join-Path $source 'WindowsRuntime.cs') (Join-Path $source 'LocalStationRuntime.cs') (Join-Path $source 'CloseSettings.cs') (Join-Path $source 'DesktopInstance.cs') (Join-Path $source 'ControllerTests.cs') (Join-Path $source 'CutoverRehearsal.cs')
 if($LASTEXITCODE -ne 0){throw 'Desktop tests did not compile.'}
 & $tests
 if($LASTEXITCODE -ne 0){throw 'Desktop controller tests failed.'}
 if($LocalStation){
  & $tests --local-cycle $OutputDirectory
  if($LASTEXITCODE -ne 0){throw 'LocalStation real process cycle failed.'}
  if($WithWeChatRuntime){
   & $tests --local-native-cycle $OutputDirectory
   if($LASTEXITCODE -ne 0){throw 'LocalStation owned WeChat native cycle failed.'}
  }
 }
 $windowTests=Join-Path $OutputDirectory 'DesktopWindowTests.exe'
 $windowArguments=@('/target:exe','/main:WechatAgent.Desktop.WindowTests',('/out:'+$windowTests))
 $windowArguments+=@($arguments | Where-Object {$_ -notlike '/target:*' -and $_ -notlike '/out:*'})
 $windowArguments+=@((Join-Path $source 'ControllerTests.cs'),(Join-Path $source 'CutoverRehearsal.cs'),(Join-Path $source 'WindowTests.cs'))
 & $compiler @windowArguments
 if($LASTEXITCODE -ne 0){throw 'Desktop window tests did not compile.'}
 $windowRunner=Join-Path $OutputDirectory 'DesktopWindowTestRunner.exe'
 & $compiler /nologo /target:exe /platform:x64 /utf8output ('/out:'+$windowRunner) /reference:System.Net.Http.dll /reference:System.Web.Extensions.dll (Join-Path $source 'Deployment.cs') (Join-Path $source 'WindowsRuntime.cs') (Join-Path $source 'LocalStationRuntime.cs') (Join-Path $source 'WindowTestRunner.cs')
 if($LASTEXITCODE -ne 0){throw 'Isolated window test runner did not compile.'}
 & $windowRunner $windowTests
 if($LASTEXITCODE -ne 0){throw 'Desktop window lifecycle tests failed.'}
 if($LocalStation){
  & $windowRunner $windowTests --local-station $OutputDirectory
  if($LASTEXITCODE -ne 0){throw 'LocalStation desktop window test failed.'}
 }
}
# Incremental desktop updates need the matching plugin launch protocol too.
$broker=Join-Path $OutputDirectory 'runtimes\windows-agent-host'
New-Item -ItemType Directory -Force -Path $broker | Out-Null
foreach($name in @('windows_lpac.py','windows_plugin_host.pyw','windows_agent_host.pyw')){Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination (Join-Path $broker $name)}
if($WithRVCBackend){
 $control=Join-Path $OutputDirectory 'runtimes\windows-station-rvc'
 $files=[ordered]@{}
 foreach($name in @('managed_music_runner.py','music_account_scope.py','process_window.py','rvc_backend.py','rvc_cover.py',
                    'rvc_pitch.py','rvc_profiles.py','rvc_recovery.py','rvc_voice.py','rvc_windows.py','singing_config.py',
                    'singing_voice.py','station_rvc.py','windows_lpac.py','yue_bridge.py','yue_recovery.py','yue_sampling.py')){
  $files[$name]=(Get-FileHash -LiteralPath (Join-Path $PSScriptRoot $name) -Algorithm SHA256).Hash.ToLowerInvariant()
 }
 $manifest=([ordered]@{schema_version=1;platform='windows-x64';policy='station-rvc-v1';files=$files}|ConvertTo-Json -Depth 4)
 Assert-NoReparsePointAncestor $control
 $manifestPath=Join-Path $control 'runtime-manifest.json'
 if(Test-Path -LiteralPath $control){
  Assert-NoReparsePointAncestor $manifestPath
  if(-not(Test-Path -LiteralPath $manifestPath -PathType Leaf) -or [IO.File]::ReadAllText($manifestPath) -cne $manifest){
   throw 'Existing standalone RVC control package differs; use a fresh output directory.'
  }
  if(@(Get-ChildItem -LiteralPath $control -Force).Count -ne ($files.Count+1)){throw 'Unexpected files in the standalone RVC control package.'}
  foreach($name in $files.Keys){
   $staged=Join-Path $control $name
   Assert-NoReparsePointAncestor $staged
   if((Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash.ToLowerInvariant() -ne $files[$name]){throw 'Standalone RVC control integrity check failed.'}
  }
 }else{
  New-Item -ItemType Directory -Path $control | Out-Null
  foreach($name in $files.Keys){
   $staged=Join-Path $control $name
   Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $staged
   if((Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash.ToLowerInvariant() -ne $files[$name]){throw 'Standalone RVC source changed during staging.'}
  }
  [IO.File]::WriteAllText($manifestPath,$manifest,[Text.UTF8Encoding]::new($false))
 }
}
# Document Workshop uses the same project-owned broker as every other local
# tool.  A normal LocalStation preview only needs the hash-pinned Python CLI;
# it deliberately carries no PDF/OCR dependencies.  Supplying the existing
# reviewed package and font source directories explicitly stages the bounded
# native PDF runtime beside that CLI.  Portable bundles use the shared resource
# preparer below, so avoid repeating either integrity-checked operation here.
if($LocalStation -and ($WithDocumentWorkshop -or $WithAudioWorkshop -or $WithRVCBackend -or $WithStationSpeech -or $WithMediaUnderstanding) -and -not $Portable){
 $runtimeRoot=Join-Path $OutputDirectory 'runtimes'
 & python -X utf8 (Join-Path $PSScriptRoot 'prepare_windows_python.py') --target (Join-Path $runtimeRoot 'windows-python-cli') --cache (Join-Path $workspace 'temp\\windows-runtime-downloads')
 if($LASTEXITCODE -ne 0){throw 'Managed Windows Python preparation failed for the selected project tools.'}
 if($hasDocumentRuntimeInput){
  $documentArgs=@('--target',(Join-Path $runtimeRoot 'document-workshop'),'--python-packages-source',$DocumentPythonPackagesDirectory,'--fonts-source',$DocumentFontsDirectory)
  if($DocumentTesseractDirectory){$documentArgs+=@('--tesseract-source',$DocumentTesseractDirectory)}
  if($DocumentPDFiumWheel){$documentArgs+=@('--pdfium-wheel',$DocumentPDFiumWheel)}
  & python -X utf8 (Join-Path $PSScriptRoot 'prepare_document_runtime.py') @documentArgs
  if($LASTEXITCODE -ne 0){throw 'Project-local native document runtime preparation failed.'}
 }
}
if($Portable){
 $resourceArgs=@{OutputDirectory=(Join-Path $OutputDirectory 'runtimes');MediaDirectory=$MediaDirectory;DocumentPythonPackagesDirectory=$DocumentPythonPackagesDirectory;DocumentTesseractDirectory=$DocumentTesseractDirectory;DocumentFontsDirectory=$DocumentFontsDirectory;DocumentPDFiumWheel=$DocumentPDFiumWheel}
 & (Join-Path $PSScriptRoot 'Prepare-WindowsResources.ps1') @resourceArgs
 if($LASTEXITCODE -ne 0){throw 'Windows runtime resources build failed.'}
 if($LocalStation){
  Copy-Item -LiteralPath (Join-Path $workspace 'desktop\windows\PORTABLE-README.txt') -Destination (Join-Path $OutputDirectory '使用说明.txt')
  Get-Item -LiteralPath $target | Select-Object FullName,Length
  Get-FileHash -LiteralPath $target -Algorithm SHA256
  return
 }
 $nativeArgs=@{OutputDirectory=(Join-Path $OutputDirectory 'runtimes\windows-x64')}
 if($NativeSourceDirectory){$nativeArgs.SourceDirectory=$NativeSourceDirectory}
 if($Test){$nativeArgs.Test=$true}
 if($GoSourcePatch){$nativeArgs.GoSourcePatch=$GoSourcePatch}
 & (Join-Path $PSScriptRoot 'Build-WindowsNative.ps1') @nativeArgs
 if($LASTEXITCODE -ne 0){throw 'Windows native build failed.'}
 $music=Join-Path $OutputDirectory 'runtimes\windows-music'
 New-Item -ItemType Directory -Force -Path $music | Out-Null
 Get-ChildItem -LiteralPath $PSScriptRoot -File -Filter '*.py' | Where-Object {$_.Name -notlike 'test_*' -and $_.Name -ne 'mobile_bridge.py'} | ForEach-Object {Copy-Item -LiteralPath $_.FullName -Destination $music}
 foreach($name in @('gpt_sovits_windows_requirements.txt')){Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $music}
 Copy-Item -LiteralPath (Join-Path $workspace 'components\yue2\app\workflows_api') -Destination (Join-Path $music 'workflows') -Recurse -Force
 $musicFiles=@{}
 Get-ChildItem -LiteralPath $music -Recurse -File | Where-Object Name -ne 'runtime-manifest.json' | ForEach-Object {$musicFiles[$_.FullName.Substring($music.Length+1)]=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLower()}
 [IO.File]::WriteAllText((Join-Path $music 'runtime-manifest.json'),(@{schema_version=1;platform='windows-x64';policy='registry-read-private-network-v1';files=$musicFiles}|ConvertTo-Json -Depth 6),[Text.UTF8Encoding]::new($false))
 Copy-Item -LiteralPath (Join-Path $workspace 'desktop\windows\PORTABLE-README.txt') -Destination (Join-Path $OutputDirectory '使用说明.txt')
}
Get-Item -LiteralPath $target | Select-Object FullName,Length
Get-FileHash -LiteralPath $target -Algorithm SHA256
