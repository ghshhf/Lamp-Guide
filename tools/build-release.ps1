param(
  [ValidatePattern('^\d+\.\d+\.\d+$')][string]$Version = '1.2.4',
  [string]$Iscc = '',
  [string]$Winres = '',
  [string]$RuntimeInstaller = ''
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$buildTools = Join-Path $projectRoot '_refs\build-tools'
$stage = Join-Path $projectRoot 'dist\app'
if (-not $Iscc) {
  $candidates = @(
    (Join-Path $buildTools 'InnoSetup6\ISCC.exe'),
    "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
    "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
  )
  $Iscc = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
}
if (-not $Winres) { $Winres = Join-Path $buildTools 'go-winres.exe' }
if (-not $RuntimeInstaller) {
  $RuntimeInstaller = Join-Path $buildTools 'MicrosoftEdgeWebView2RuntimeInstallerX64.exe'
}
foreach ($required in @($Iscc, $Winres, $RuntimeInstaller)) {
  if (-not $required -or -not (Test-Path -LiteralPath $required)) {
    throw 'Missing packaging tool or WebView2 runtime. See packaging/README.md.'
  }
}
$runtimeSignature = Get-AuthenticodeSignature -LiteralPath $RuntimeInstaller
if ($runtimeSignature.Status -ne 'Valid' -or $runtimeSignature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation') {
  throw 'WebView2 must be an unmodified installer with a valid Microsoft signature.'
}
New-Item -ItemType Directory -Path $stage -Force | Out-Null
Push-Location $projectRoot
$previousArch = $env:GOARCH
$previousOS = $env:GOOS
try {
  $env:GOARCH = 'amd64'
  $env:GOOS = 'windows'
  & node tools/build-assets.cjs
  if ($LASTEXITCODE) { throw 'Icon rendering failed.' }
  & $Winres make --in packaging/winres.json --arch amd64 --out rsrc --file-version "$Version.0" --product-version "$Version.0"
  if ($LASTEXITCODE) { throw 'Windows resource compilation failed.' }
  $linkFlags = "-H=windowsgui -s -w -X zenui/internal/zen/gateway.Version=$Version -X zenui/internal/zen/update.Current=$Version"
  & go build -trimpath -ldflags $linkFlags -o (Join-Path $stage 'LampGuide.exe') .
  if ($LASTEXITCODE) { throw 'Application build failed.' }
  Copy-Item -LiteralPath (Join-Path $projectRoot 'packaging\使用说明.txt') -Destination $stage -Force
  & node tools/third-party-notices.cjs (Join-Path $stage 'THIRD-PARTY-NOTICES.txt')
  if ($LASTEXITCODE) { throw 'Third-party notices failed.' }
  & $Iscc "/DAppVersion=$Version" "/DStageDir=$stage" "/DRuntimeInstaller=$RuntimeInstaller" packaging/LampGuide.iss
  if ($LASTEXITCODE) { throw 'Installer compilation failed.' }
  $setup = Join-Path $projectRoot "dist\LampGuide-Setup-$Version-win-x64.exe"
  $checksum = (Get-FileHash -LiteralPath $setup -Algorithm SHA256).Hash.ToLowerInvariant()
  [IO.File]::WriteAllText("$setup.sha256", "$checksum  $([IO.Path]::GetFileName($setup))`n", [Text.UTF8Encoding]::new($false))
  Get-Item -LiteralPath $setup | Select-Object FullName,Length
} finally {
  $env:GOARCH = $previousArch
  $env:GOOS = $previousOS
  Pop-Location
}
