"""Exercise the PowerShell shutdown sequence with the observed delayed exit."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(shutil.which('pwsh'), 'PowerShell required')
class ShutdownTest(unittest.TestCase):
    def test_original_process_exit_precedes_unregister(self):
        helper=pathlib.Path(__file__).resolve().parents[3]/'images/windows/cache-runtime.ps1'
        script=r'''
param($Helper,$Executable)
. $Helper
$script:exits=$true;$script:wrongOwner=$false;$script:waited=$false
function Get-CimInstance { @{Name='cldt-image-cache';PathName='containerd --config owned.toml';ProcessId=123} }
function Get-Process {
 param($Name,$Id,$ErrorAction)
 if($Id) {
  $p=[pscustomobject]@{Path=$(if($script:wrongOwner){'other.exe'}else{$Executable})}
  $p|Add-Member ScriptMethod WaitForExit {param($timeout);if($timeout -ne 30000){throw 'wrong bound'};$script:waited=$true;return $script:exits}
  return $p
 }
}
function Stop-Service {param($Name)}
function Get-Service {param($Name,$ErrorAction)}
$handle=@{executable=$Executable;config='owned.toml'}
$result=Stop-ImageCacheRuntime $handle
if(!$result.serviceStopped -or !$result.cacheServiceRemoved -or !$script:waited -or !$global:unregistered){throw 'shutdown not verified'}
$script:exits=$false;$global:unregistered=$false
try {Stop-ImageCacheRuntime $handle;throw 'unexpected success'} catch {if($_ -notmatch 'did not exit within 30 seconds'){throw}}
if($global:unregistered){throw 'unregistered before original process exited'}
$script:wrongOwner=$true;$script:waited=$false
try {Stop-ImageCacheRuntime $handle;throw 'unexpected success'} catch {if($_ -notmatch 'ownership changed'){throw}}
if($script:waited){throw 'waited on unrelated process'}
'''
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root);(p/'test.ps1').write_text(script)
            (p/'unregister.ps1').write_text('$global:unregistered=$true; $global:LASTEXITCODE=0')
            r=subprocess.run(['pwsh','-NoProfile','-File',str(p/'test.ps1'),str(helper),str(p/'unregister.ps1')],capture_output=True,text=True,timeout=30)
            self.assertEqual(r.returncode,0,r.stdout+r.stderr)

    def test_native_unpacked_images_do_not_override_failed_shutdown(self):
        import json
        import sys
        sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[3]))
        from images.windows.cache import verify_preparation
        f=json.loads((pathlib.Path(__file__).parent/'testdata/windows-cache-shutdown-race.json').read_text())
        self.assertEqual(len(f['record']['snapshot']['readyImages']),16)
        self.assertTrue(f['record']['cacheServiceRemoved'])
        self.assertFalse(f['record']['serviceStopped'])
        with self.assertRaises(ValueError):verify_preparation(f['record'],f['recipe'],'2025')
