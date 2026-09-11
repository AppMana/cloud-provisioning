# Query every HNS network through the HCN v2 API, one network at a time.
#
# Felix and the CNI plugin use HCN v2 (computenetwork.dll). Its enumeration
# succeeds and then a single HcnQueryNetworkProperties call can fail for one
# network with 0x803b001b "Invalid JSON document string" while the v1
# Get-HnsNetwork listing looks healthy. Felix logs the failure without the
# network ID, so this reports, per network, the ID, the v1 name and type, and
# either the v2 document length or the exact HCN error record.
#
# Read-only. Run as Administrator on the Windows node:
#   powershell -NoProfile -ExecutionPolicy Bypass -File hcn_networks.ps1

$ErrorActionPreference = 'Stop'

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class Hcn {
    [DllImport("computenetwork.dll", CharSet = CharSet.Unicode)]
    public static extern int HcnEnumerateNetworks(string query, [MarshalAs(UnmanagedType.LPWStr)] out string networks, [MarshalAs(UnmanagedType.LPWStr)] out string errorRecord);
    [DllImport("computenetwork.dll", CharSet = CharSet.Unicode)]
    public static extern int HcnOpenNetwork(ref Guid id, out IntPtr network, [MarshalAs(UnmanagedType.LPWStr)] out string errorRecord);
    [DllImport("computenetwork.dll", CharSet = CharSet.Unicode)]
    public static extern int HcnQueryNetworkProperties(IntPtr network, string query, [MarshalAs(UnmanagedType.LPWStr)] out string properties, [MarshalAs(UnmanagedType.LPWStr)] out string errorRecord);
    [DllImport("computenetwork.dll")]
    public static extern int HcnCloseNetwork(IntPtr network);
}
"@

$v1 = @{}
foreach ($n in (Get-HnsNetwork)) { $v1[[string]$n.Id] = $n }

$ids = $null; $err = $null
$rc = [Hcn]::HcnEnumerateNetworks('{"SchemaVersion":{"Major":2,"Minor":0}}', [ref]$ids, [ref]$err)
if ($rc -ne 0) { throw ("HcnEnumerateNetworks failed 0x{0:x8}: {1}" -f $rc, $err) }

$report = @()
foreach ($id in ($ids | ConvertFrom-Json)) {
    $guid = [Guid]$id
    $handle = [IntPtr]::Zero; $err = $null
    $row = [ordered]@{ id = $id; name = $null; type = $null; v2Bytes = $null; error = $null }
    $known = $v1[$id.ToUpper()]
    if (-not $known) { $known = $v1[$id.ToLower()] }
    if ($known) { $row.name = $known.Name; $row.type = $known.Type }
    $rc = [Hcn]::HcnOpenNetwork([ref]$guid, [ref]$handle, [ref]$err)
    if ($rc -ne 0) {
        $row.error = ("HcnOpenNetwork 0x{0:x8}: {1}" -f $rc, $err)
    } else {
        $doc = $null; $err = $null
        $rc = [Hcn]::HcnQueryNetworkProperties($handle, '{"SchemaVersion":{"Major":2,"Minor":0}}', [ref]$doc, [ref]$err)
        if ($rc -ne 0) {
            $row.error = ("HcnQueryNetworkProperties 0x{0:x8}: {1}" -f $rc, $err)
        } else {
            $row.v2Bytes = $doc.Length
        }
        [void][Hcn]::HcnCloseNetwork($handle)
    }
    $report += [pscustomobject]$row
}
$report | ConvertTo-Json -Depth 3
