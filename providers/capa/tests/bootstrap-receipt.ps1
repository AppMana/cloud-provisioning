# Exercise the real consumer on Windows using synthetic in-memory secret chunks.
# No CAPA userdata is replayed and no Secrets Manager request is made.
param([Parameter(Mandatory=$true)][string]$ConsumerPath)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
if ([Environment]::OSVersion.Version.Build -notin @(20348,26100)) { throw 'Native Windows 2022 or 2025 is required' }
$consumer=Get-Content -LiteralPath $ConsumerPath -Raw
$parent='C:\ProgramData\CloudProvisioning'
$testRoot=Join-Path $parent ('ReceiptTest-'+[Guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $testRoot
$results=@()
try {
 foreach($case in @('success','script-failure')) {
  $rootPath=Join-Path $testRoot $case
  $payload="Write-Output 'synthetic bootstrap succeeded'"
  if($case -eq 'script-failure'){$payload="throw 'PRIVATE_SENTINEL synthetic bootstrap failure'"}
  $stream=New-Object IO.MemoryStream
  $gzip=New-Object IO.Compression.GZipStream($stream,[IO.Compression.CompressionMode]::Compress,$true)
  $bytes=[Text.Encoding]::UTF8.GetBytes($payload)
  $gzip.Write($bytes,0,$bytes.Length);$gzip.Dispose()
  $compressedFixture=[Convert]::ToBase64String($stream.ToArray());$stream.Dispose()
  $settings=@{prefix='aws.cluster.x-k8s.io/receipt-contract-test';chunks=1;region='us-west-2'} | ConvertTo-Json -Compress
  $source=$consumer.Replace('__SETTINGS__',[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($settings)))
  $source=$source.Replace("`$root = 'C:\ProgramData\CloudProvisioning\Bootstrap'","`$root = '$rootPath'")
  $moduleLine='Import-Module AWS.Tools.SecretsManager -RequiredVersion 5.0.268 -ErrorAction Stop'
  $stubs=@'
function Get-SECSecretValue {
 param($SecretId,$Region,$Credential,$Select)
 @{SecretBinary=(New-Object IO.MemoryStream(,[Convert]::FromBase64String($compressedFixture)))}
}
function Remove-SECSecret { param($SecretId,$Region,$Credential,$DeleteWithNoRecovery,[switch]$Force) }
'@
  if(!$source.Contains($moduleLine)){throw 'Consumer import seam changed'}
  $source=$source.Replace($moduleLine,$moduleLine+"`n"+$stubs)
  $failed=$false;$failureCategory=$null
  try { & ([ScriptBlock]::Create($source)) *> $null } catch {$failed=$true;$failureCategory=$_.FullyQualifiedErrorId}
  $receiptPath=Join-Path $rootPath 'status.json'
  $raw=Get-Content -LiteralPath $receiptPath -Raw
  $receipt=$raw | ConvertFrom-Json
  $expectedState='succeeded';$expectedStage='complete'
  if($case -eq 'script-failure'){$expectedState='failed';$expectedStage='executing'}
  $checks=@{
   expectedExit=($failed -eq ($case -eq 'script-failure'))
   expectedState=($receipt.state -eq $expectedState)
   expectedStage=($receipt.stage -eq $expectedStage)
   validSchema=($receipt.schemaVersion -eq 1)
   instanceBound=($receipt.instanceID -match '^i-[0-9a-f]+$')
   bootstrapBound=($receipt.bootstrapID -eq '2bb99d77219183e0c777fd977dfc067b0cd1b031ebfc20b66c67458260b404ea')
   noPrivateError=(!$raw.Contains('PRIVATE_SENTINEL'))
   payloadRemoved=(!(Test-Path -LiteralPath (Join-Path $rootPath 'join.ps1')))
   noTemporaryReceipt=(@(Get-ChildItem -LiteralPath $rootPath -Filter '*.tmp').Count -eq 0)
   protectedDirectory=(Get-Acl $rootPath).AreAccessRulesProtected
  }
  $results+=@{case=$case;checks=$checks;receipt=$receipt;failureCategory=$failureCategory}
  if(@($checks.Values | Where-Object {$_ -ne $true}).Count){
   @{build=[Environment]::OSVersion.Version.Build;cases=$results;passed=$false} | ConvertTo-Json -Depth 8 -Compress
   throw ('Receipt contract failed: '+$case)
  }
 }
 @{build=[Environment]::OSVersion.Version.Build;cases=$results;scope='Synthetic consumer inputs on native Windows; no live CAPA claim'} | ConvertTo-Json -Depth 8 -Compress
} finally { Remove-Item -LiteralPath $testRoot -Recurse -Force }
