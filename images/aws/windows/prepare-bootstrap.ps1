# AWS-specific image layer. Run while building, never at worker startup.
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$version='5.0.268'
$packages=@{
 'AWS.Tools.Common'='6cf06ef24b5bbc714c490ddb40dd6b0e65dab0653d577f815b88da5037516ab8'
 'AWS.Tools.SecretsManager'='b4312531b98a3b4007a7b4ddaeda5d1b823c0880702d26f327f3a19516c50cef'
}
$ProgressPreference='SilentlyContinue'
foreach ($name in @('AWS.Tools.Common','AWS.Tools.SecretsManager')) {
 $package=Join-Path $env:TEMP ($name+'.zip')
 Invoke-WebRequest -UseBasicParsing -Uri "https://www.powershellgallery.com/api/v2/package/$name/$version" -OutFile $package
 if ((Get-FileHash $package -Algorithm SHA256).Hash.ToLowerInvariant() -ne $packages[$name]) { throw "Package checksum mismatch: $name" }
 $destination=Join-Path "$env:ProgramFiles\WindowsPowerShell\Modules" "$name\$version"
 $stage=Join-Path $env:TEMP ([Guid]::NewGuid().ToString())
 try {
  Expand-Archive -Path $package -DestinationPath $stage
  if (Test-Path $destination) {
   # Amazon images may already contain these modular AWS Tools. Verify their
   # executable files against the pinned package instead of overwriting them.
   foreach ($file in Get-ChildItem $stage -Recurse -File | Where-Object { $_.Extension -in '.dll','.psd1','.psm1','.ps1' }) {
    $relative=$file.FullName.Substring($stage.Length+1)
    $installed=Join-Path $destination $relative
    if (!(Test-Path $installed) -or (Get-FileHash $installed).Hash -ne (Get-FileHash $file.FullName).Hash) { throw "Installed module differs from pinned package: $name/$relative" }
   }
  } else {
   $null=New-Item -ItemType Directory -Force (Split-Path $destination)
   Move-Item $stage $destination
  }
 } finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $stage
  Remove-Item -Force -ErrorAction SilentlyContinue $package
 }
}
Import-Module AWS.Tools.SecretsManager -RequiredVersion $version -ErrorAction Stop
if (!(Get-Command Get-SECSecretValue) -or !(Get-Command Remove-SECSecret)) { throw 'Secrets Manager cmdlets unavailable' }
