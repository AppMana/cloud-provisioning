<# Export bounded same-host VFP context from an already stopped ETL. #>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$Path,
    [Parameter(Mandatory=$true)][string]$Output,
    [Parameter(Mandatory=$true)][DateTimeOffset]$StartUtc,
    [Parameter(Mandatory=$true)][DateTimeOffset]$EndUtc,
    [ValidateRange(1,10000)][int]$MaxEvents = 1000,
    [ValidateCount(0,20)][ValidateRange(0,65535)][int[]]$EventId = @()
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($EndUtc -le $StartUtc -or ($EndUtc - $StartUtc).TotalSeconds -gt 1) {
    throw 'Supply an ordered UTC window of at most one second'
}
$inputFile = Get-Item -LiteralPath $Path
if ($inputFile.PSIsContainer -or ($inputFile.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
    throw 'Expected a regular retained ETL file'
}
if (![IO.Path]::IsPathRooted($Output) -or (Test-Path -LiteralPath $Output)) {
    throw 'Supply a new absolute output filename'
}
$beforeHash = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
$stream = [IO.File]::Open($Output, [IO.FileMode]::CreateNew)
$gzip = New-Object IO.Compression.GZipStream($stream, [IO.Compression.CompressionMode]::Compress)
$writer = New-Object IO.StreamWriter($gzip, (New-Object Text.UTF8Encoding($false)))
$count = 0
$queried = 0
$outsideWindow = 0
try {
    # Native Server 2022/2025 ETLs returned no records for a combined Path,
    # ProviderName and Id hashtable query even when an XPath Id query found
    # the same records. Use XPath for time and check the provider GUID directly.
    $start = $StartUtc.UtcDateTime.ToString('o', [Globalization.CultureInfo]::InvariantCulture)
    $end = $EndUtc.UtcDateTime.ToString('o', [Globalization.CultureInfo]::InvariantCulture)
    $idPredicate = if ($EventId.Count) {
        ' and (' + (($EventId | Sort-Object -Unique | ForEach-Object { "EventID=$_" }) -join ' or ') + ')'
    } else { '' }
    $xpath = "*[System[TimeCreated[@SystemTime >= '$start' and @SystemTime <= '$end']$idPredicate]]"
    Get-WinEvent -Path $inputFile.FullName -FilterXPath $xpath -Oldest -MaxEvents $MaxEvents -ErrorAction SilentlyContinue -ErrorVariable queryErrors |
        ForEach-Object {
            $queried++
            # Native ETL XPath selection can include records just before a
            # sub-millisecond lower bound. Enforce the exact UTC bounds here.
            $time = $_.TimeCreated.ToUniversalTime()
            if ($time -lt $StartUtc.UtcDateTime -or $time -gt $EndUtc.UtcDateTime) {
                $outsideWindow++
            } elseif ($_.ProviderId -eq [Guid]'9f2660ea-cfe7-428f-9850-aeca612619b0' -and
                (!$EventId.Count -or $_.Id -in $EventId)) {
                $writer.WriteLine((@{xml=$_.ToXml()} | ConvertTo-Json -Compress))
                $count++
            }
        }
    if (@($queryErrors | Where-Object {$_.FullyQualifiedErrorId -notlike 'NoMatchingEventsFound*'}).Count) {
        throw 'Native VFP event query failed; retain the original ETL and partial output'
    }
} finally { $writer.Dispose(); $gzip.Dispose(); $stream.Dispose() }
if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $beforeHash) {
    throw 'Input ETL changed during export'
}
[ordered]@{
    startUtc=$StartUtc.ToUniversalTime().ToString('o'); endUtc=$EndUtc.ToUniversalTime().ToString('o')
    sourceSHA256=$beforeHash; sourceUnchanged=$true
    eventCount=$count; queriedEventCount=$queried
    outsideWindowEventCount=$outsideWindow
    maxEvents=$MaxEvents; eventLimitReached=($queried -ge $MaxEvents)
    eventIds=@($EventId | Sort-Object -Unique)
    artifact=@{name=[IO.Path]::GetFileName($Output); bytes=(Get-Item -LiteralPath $Output).Length
        sha256=(Get-FileHash -LiteralPath $Output -Algorithm SHA256).Hash.ToLowerInvariant()}
    scope='Same-host event-time context only; no packet identity, event-loss audit or network qualification.'
} | ConvertTo-Json -Depth 4 -Compress
