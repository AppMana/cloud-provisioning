$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$settings = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__SETTINGS__')) | ConvertFrom-Json
$parent = 'C:\ProgramData\CloudProvisioning'
if (!(Get-Acl $parent).AreAccessRulesProtected -or ((Get-Item $parent).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Protected image directory is required' }
$root = 'C:\ProgramData\CloudProvisioning\Bootstrap'
if ((Test-Path $root) -and ((Get-Item $root).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Bootstrap directory cannot be a reparse point' }
$null = New-Item -ItemType Directory -Force -Path $root
$acl = New-Object Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($sid in @('S-1-5-18','S-1-5-32-544')) {
    $identity = New-Object Security.Principal.SecurityIdentifier($sid)
    $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity,'FullControl','ContainerInherit,ObjectInherit','None','Allow')
    $acl.AddAccessRule($rule)
}
Set-Acl -Path $root -AclObject $acl
$payload = Join-Path $root 'join.ps1'
# The detached launch agent cannot report this consumer's terminal result.
# Publish only non-secret state, bound to the actual EC2 instance and chunk set.
$instanceID=$null
$bootstrapID=$null
$stage='initializing'
function Write-BootstrapReceipt([string]$state,[string]$phase) {
    if ($instanceID -notmatch '^i-[0-9a-f]+$' -or $bootstrapID -notmatch '^[0-9a-f]{64}$') { throw 'Bootstrap receipt identity is unavailable' }
    $record=[ordered]@{schemaVersion=1;instanceID=$instanceID;bootstrapID=$bootstrapID;state=$state;stage=$phase;updatedAt=[DateTime]::UtcNow.ToString('o')}
    $destination=Join-Path $root 'status.json'
    $temporary=Join-Path $root ([Guid]::NewGuid().ToString('N')+'.tmp')
    try {
        [IO.File]::WriteAllText($temporary,($record | ConvertTo-Json -Compress),(New-Object Text.UTF8Encoding($false)))
        if (Test-Path -LiteralPath $destination) { [IO.File]::Replace($temporary,$destination,[NullString]::Value) }
        else { [IO.File]::Move($temporary,$destination) }
    } finally { Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue }
}
try {
    $metadataToken=Invoke-RestMethod -Method Put -Uri 'http://169.254.169.254/latest/api/token' -Headers @{'X-aws-ec2-metadata-token-ttl-seconds'='60'} -TimeoutSec 10
    $instanceID=(Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/instance-id' -Headers @{'X-aws-ec2-metadata-token'=$metadataToken} -TimeoutSec 10).Trim()
    $metadataToken=$null
    $sha=[Security.Cryptography.SHA256]::Create()
    try { $bootstrapID=([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($settings.prefix)))).Replace('-','').ToLowerInvariant() }
    finally { $sha.Dispose() }
    Write-BootstrapReceipt 'running' $stage
    $credentials=$null
    $compressed=$null
    try {
        Import-Module AWS.Tools.SecretsManager -RequiredVersion 5.0.268 -ErrorAction Stop
        # Explicitly use the instance profile, never a saved profile or environment key.
        $credentials=New-Object Amazon.Runtime.InstanceProfileAWSCredentials
        $compressed=New-Object IO.MemoryStream
        $stage='downloading'
        Write-BootstrapReceipt 'running' $stage
        for ($index=0; $index -lt $settings.chunks; $index++) {
            $secretId = $settings.prefix + '-' + $index
            $response = $null
            for ($attempt=0; $attempt -lt 10; $attempt++) {
                try {
                    $response = Get-SECSecretValue -SecretId $secretId -Region $settings.region -Credential $credentials -Select '*'
                    break
                } catch {
                    if ($attempt -eq 9) { throw 'Unable to retrieve bootstrap secret chunk' }
                    Start-Sleep -Seconds 10
                }
            }
            if ($null -eq $response.SecretBinary) { throw 'Bootstrap chunk is not binary' }
            $response.SecretBinary.CopyTo($compressed)
            $response.SecretBinary.Dispose()
        }
        $compressed.Position=0
        $gzip=New-Object IO.Compression.GZipStream($compressed,[IO.Compression.CompressionMode]::Decompress,$true)
        $reader=New-Object IO.StreamReader($gzip,(New-Object Text.UTF8Encoding($false,$true)))
        try { $script=$reader.ReadToEnd() } finally { $reader.Dispose() }
        if ([string]::IsNullOrWhiteSpace($script)) { throw 'Bootstrap script is empty' }
        # Windows PowerShell 5 needs a BOM to read a UTF-8 script file correctly.
        [IO.File]::WriteAllText($payload,$script,(New-Object Text.UTF8Encoding($true)))
        $script=$null
        $stage='deleting-secrets'
        Write-BootstrapReceipt 'running' $stage
        # Delete all encrypted chunks before invoking the credential-bearing script.
        for ($index=0; $index -lt $settings.chunks; $index++) {
            $null=Remove-SECSecret -SecretId ($settings.prefix+'-'+$index) -Region $settings.region -Credential $credentials -DeleteWithNoRecovery $true -Force
        }
        $stage='executing'
        Write-BootstrapReceipt 'running' $stage
        & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $payload
        $code=$LASTEXITCODE
        if ($code -ne 0) { throw "Bootstrap script exited $code" }
        $stage='cleanup'
    } finally {
        if ($null -ne $compressed) { $compressed.Dispose() }
        if ($null -ne $credentials) { $credentials.Dispose() }
        if (Test-Path -LiteralPath $payload) { Remove-Item -LiteralPath $payload -Force -ErrorAction Stop }
    }
    Write-BootstrapReceipt 'succeeded' 'complete'
} catch {
    $failure=$_
    # Never serialize an exception: it can contain credential-bearing arguments.
    try { Write-BootstrapReceipt 'failed' $stage } catch { }
    throw $failure
}
