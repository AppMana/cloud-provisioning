# Exercise input guards without contacting IMDS or changing host networking.
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$tokens=$null;$errors=$null
$ast=[Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'configure-cni.ps1'),[ref]$tokens,[ref]$errors)
if ($errors.Count) { throw 'CNI initializer has parse errors' }
foreach ($name in @('IPv4','Prefix','Contains')) {
    $definition=$ast.Find({param($n) $n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$false)
    if (!$definition) { throw 'Input guard missing' }
    . ([ScriptBlock]::Create($definition.Extent.Text))
}
$checks=0
foreach ($value in @('127.1','0x7f000001','::1','999.1.1.1','10.0.0.1/24')) {
    $rejected=$false
    try { $null=IPv4 $value } catch { $rejected=$true }
    if (!$rejected) { throw "Accepted non-IPv4 input: $value" }
    $checks++
}
foreach ($value in @('0.0.0.0/0','10.0.0.1/24','10.0.0.0/33','10.0.0.0','10.0.0.0/-1')) {
    $rejected=$false
    try { $null=Prefix $value } catch { $rejected=$true }
    if (!$rejected) { throw "Accepted invalid prefix: $value" }
    $checks++
}
foreach ($case in @(
    @('172.29.0.0/25','172.29.0.127',$true),
    @('172.29.0.0/25','172.29.0.128',$false),
    @('10.96.0.0/12','10.96.0.10',$true),
    @('10.96.0.0/12','10.112.0.10',$false),
    @('192.0.2.1/32','192.0.2.1',$true),
    @('192.0.2.1/32','192.0.2.2',$false)
)) {
    if ((Contains (Prefix $case[0]) (IPv4 $case[1])) -ne $case[2]) { throw 'Incorrect prefix membership' }
    $checks++
}
"Passed $checks IPv4 input and prefix boundary checks"
