param([Parameter(Mandatory=$true)][string]$OutputDirectory,[string]$MediaDirectory,[string]$DocumentPythonPackagesDirectory,[string]$DocumentTesseractDirectory,[string]$DocumentFontsDirectory, [string]$DocumentPDFiumWheel)
$ErrorActionPreference='Stop'
$workspace=Split-Path $PSScriptRoot -Parent
$OutputDirectory=[IO.Path]::GetFullPath($OutputDirectory)
& python -X utf8 (Join-Path $PSScriptRoot 'prepare_windows_python.py') --target (Join-Path $OutputDirectory 'windows-python-cli') --cache (Join-Path $workspace 'temp\windows-runtime-downloads')
if($LASTEXITCODE -ne 0){throw 'Managed Windows Python preparation failed.'}
if(-not $MediaDirectory){$MediaDirectory=Split-Path (Get-Command ffmpeg.exe -ErrorAction Stop).Source -Parent}
$MediaDirectory=[IO.Path]::GetFullPath($MediaDirectory)
$mediaTarget=Join-Path $OutputDirectory 'windows-media'
if(Test-Path -LiteralPath $mediaTarget){
 $existing=Get-Content -LiteralPath (Join-Path $mediaTarget 'runtime-manifest.json') -Raw | ConvertFrom-Json
 foreach($entry in $existing.files.PSObject.Properties){
  if($entry.Name -ne [IO.Path]::GetFileName($entry.Name) -or (Get-FileHash -LiteralPath (Join-Path $mediaTarget $entry.Name) -Algorithm SHA256).Hash -ne $entry.Value){throw 'Existing media runtime failed integrity checks; it was not overwritten.'}
 }
 foreach($name in @('ffmpeg.exe','ffprobe.exe')){
  if((Get-FileHash -LiteralPath (Join-Path $MediaDirectory $name) -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath (Join-Path $mediaTarget $name) -Algorithm SHA256).Hash){throw 'Different media runtime already exists; build into a new output directory for review and atomic deployment.'}
 }
 Write-Output 'Existing project-local resources verified; no files overwritten.'
 return
}
$media=Join-Path $OutputDirectory ('windows-media.staging-'+[Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $media | Out-Null
foreach($name in @('ffmpeg.exe','ffprobe.exe')){
 $source=Join-Path $MediaDirectory $name
 if(-not(Test-Path -LiteralPath $source)){throw 'The build requires FFmpeg and FFprobe from the same distribution.'}
 Copy-Item -LiteralPath $source -Destination (Join-Path $media $name)
}
# Shared FFmpeg distributions carry their codec DLLs beside the tools.
Get-ChildItem -LiteralPath $MediaDirectory -Filter '*.dll' -File | ForEach-Object {Copy-Item -LiteralPath $_.FullName -Destination $media}
$licenseRoots=@($MediaDirectory,(Split-Path $MediaDirectory -Parent))
$licenses=@(foreach($base in $licenseRoots){Get-ChildItem -LiteralPath $base -File | Where-Object {$_.Name -match '^(LICENSE|COPYING|README)(\.|$)'}})
if($licenses.Count -eq 0){throw 'FFmpeg distribution license/source information is required; no unlicensed package produced.'}
foreach($item in $licenses){Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $media $item.Name)}
$version=& (Join-Path $media 'ffmpeg.exe') -version
if($LASTEXITCODE -ne 0){throw 'Project-local FFmpeg failed its startup check.'}
& (Join-Path $media 'ffprobe.exe') -version | Out-Null
if($LASTEXITCODE -ne 0){throw 'Project-local FFprobe failed its startup check.'}
$files=@{}
Get-ChildItem -LiteralPath $media -File | Where-Object Name -ne 'runtime-manifest.json' | ForEach-Object {$files[$_.Name]=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLower()}
$record=@{schema_version=1;platform='windows-x64';version=$version[0];build_configuration=$version;files=$files}
[IO.File]::WriteAllText((Join-Path $media 'runtime-manifest.json'),($record|ConvertTo-Json -Depth 6),[Text.UTF8Encoding]::new($false))
Move-Item -LiteralPath $media -Destination $mediaTarget
if($DocumentPythonPackagesDirectory -or $DocumentFontsDirectory -or $DocumentTesseractDirectory -or $DocumentPDFiumWheel){
 if(-not $DocumentPythonPackagesDirectory -or -not $DocumentFontsDirectory){throw 'Native document runtime requires both DocumentPythonPackagesDirectory and DocumentFontsDirectory.'}
 $documentArgs=@('--target',(Join-Path $OutputDirectory 'document-workshop'),'--python-packages-source',$DocumentPythonPackagesDirectory,'--fonts-source',$DocumentFontsDirectory)
 if($DocumentTesseractDirectory){$documentArgs+=@('--tesseract-source',$DocumentTesseractDirectory)}
 if($DocumentPDFiumWheel){$documentArgs+=@('--pdfium-wheel',$DocumentPDFiumWheel)}
 & python -X utf8 (Join-Path $PSScriptRoot 'prepare_document_runtime.py') @documentArgs
 if($LASTEXITCODE -ne 0){throw 'Project-local document runtime preparation failed.'}
}
Write-Output 'Project-local Python and media resources prepared; user data was not changed.'
