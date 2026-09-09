<#
Bounded VFP ETW collection on a disposable Windows VM. Run as Administrator.
The caller owns the new output directory and must collect and remove it after
verifying hashes. No forwarding rules, flow settings or trace filters change.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$Directory,
    [ValidateRange(1,60)][int]$Seconds = 15,
    [switch]$ExportXml,
    [ValidateRange(1,100000)][int]$MaxXmlEvents = 10000,
    [switch]$IncludeControl,
    [switch]$FlowControlOnly,
    [ValidateRange(16,256)][int]$MaxFileMiB = 64
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$provider = 'Microsoft-Windows-Hyper-V-VfpExt'
$vfp = "$env:SystemRoot\System32\vfpctrl.exe"
if ($FlowControlOnly -and $IncludeControl) {
    throw 'Choose either focused flow/control validation or the broader control profile'
}
if (!(Test-Path -LiteralPath $vfp)) { throw 'VFP control tool is missing' }
if (![IO.Path]::IsPathRooted($Directory) -or (Test-Path -LiteralPath $Directory)) {
    throw 'Supply a new absolute evidence directory'
}
$before = (& $vfp /get-trace-filtering-state | Out-String)
if ($LASTEXITCODE -ne 0 -or $before -notmatch 'Trace filtering is Disabled') {
    throw 'Existing trace filtering requires a separate capture plan'
}
$session = 'cldt-vfp-' + [Guid]::NewGuid().ToString('N')
New-Item -ItemType Directory -Path $Directory | Out-Null
$etl = Join-Path $Directory 'vfp.etl'
$utf8 = New-Object Text.UTF8Encoding($false)
$meta = Get-WinEvent -ListProvider $provider
$metadata = [ordered]@{
    name=$meta.Name; id=$meta.Id.ToString()
    keywords=@($meta.Keywords | Select-Object Name,Value)
    events=@($meta.Events | Select-Object Id,Version,Description,Template)
}
[IO.File]::WriteAllText((Join-Path $Directory 'provider.json'),
    ($metadata | ConvertTo-Json -Depth 8 -Compress), $utf8)
$started = $false
$keywords = if ($FlowControlOnly) { '0xc80000' } elseif ($IncludeControl) { '0xcd0e0f' } else { '0x8c0e0f' }
$startAt = [DateTime]::UtcNow.ToString('o')
try {
    # Intercept, Guard, Rule, Transform, PaRoute, MapEncap, Forward,
    # Packet, UnifiedFlow and Flow, as advertised by the native provider.
    # Control and ControlValidation add NDIS control and IOCTL/object-processing
    # events respectively; the provider advertises them as separate keywords.
    # The focused profile selects only UnifiedFlow, Flow and ControlValidation
    # to preserve flow lifetime and IOCTL context without per-layer/timer volume.
    $startOutput = (& logman start $session -ets -o $etl -p $provider $keywords 5 -f bincirc -max $MaxFileMiB -bs 64 -nb 16 64 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "ETW start failed: $startOutput" }
    $started = $true
    Start-Sleep -Seconds $Seconds
} finally {
    if ($started) {
        $stopOutput = (& logman stop $session -ets | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "ETW stop failed for $session : $stopOutput" }
    }
}
$stopAt = [DateTime]::UtcNow.ToString('o')
$after = (& $vfp /get-trace-filtering-state | Out-String)
if ($LASTEXITCODE -ne 0 -or $after -ne $before) { throw 'Trace filtering state changed during observation' }
$counts = @{}
$eventCount = 0
$artifactNames = @('provider.json','vfp.etl')
if ($ExportXml) {
    $file = [IO.File]::Open((Join-Path $Directory 'events.jsonl.gz'), [IO.FileMode]::CreateNew)
    $gzip = New-Object IO.Compression.GZipStream($file, [IO.Compression.CompressionMode]::Compress)
    $writer = New-Object IO.StreamWriter($gzip, $utf8)
    try {
        Get-WinEvent -Path $etl -Oldest -MaxEvents $MaxXmlEvents | ForEach-Object {
            $key = $_.ProviderName + '/' + $_.Id + '/' + $_.Version
            if (!$counts.ContainsKey($key)) { $counts[$key] = 0 }
            $counts[$key]++
            $eventCount++
            $writer.WriteLine((@{xml=$_.ToXml()} | ConvertTo-Json -Compress))
        }
    } finally { $writer.Dispose(); $gzip.Dispose(); $file.Dispose() }
    $artifactNames += 'events.jsonl.gz'
}
$files = $artifactNames | ForEach-Object {
    $path = Join-Path $Directory $_
    @{name=$_; bytes=(Get-Item -LiteralPath $path).Length; sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()}
}
$result = [ordered]@{
    build=[Environment]::OSVersion.Version.Build; session=$session
    startedAt=$startAt; stoppedAt=$stopAt; requestedSeconds=$Seconds
    traceStopped=$true; traceFilteringUnchanged=$true
    keywordMask=$keywords; includesControl=[bool]$IncludeControl
    includesControlValidation=([bool]$IncludeControl -or [bool]$FlowControlOnly)
    flowControlOnly=[bool]$FlowControlOnly; maxFileMiB=$MaxFileMiB
    xmlExported=[bool]$ExportXml; xmlEventLimit=$MaxXmlEvents
    xmlEventLimitReached=([bool]$ExportXml -and $eventCount -ge $MaxXmlEvents)
    eventCount=$eventCount; counts=$counts; artifacts=@($files)
    qualificationEligible=$false
    scope='Bounded VFP event collection; circular capture may overwrite events. Event loss and packet identity require independent validation.'
}
$json = $result | ConvertTo-Json -Depth 5 -Compress
[IO.File]::WriteAllText((Join-Path $Directory 'result.json'), $json, $utf8)
$json
