package aws

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"io/fs"
	"strings"
)

// GuestCommands owns guest command syntax. EC2 lifecycle and SSM polling are
// shared by Linux and Windows; neither needs to infer an OS from an AMI name.
type GuestCommands interface {
	BootstrapFailure(context.Context, rig.Node) error
	Document() string
	Exec([]string) string
	Pipe(string, []string) string
	Put(string, string, fs.FileMode) string
}

type LinuxCommands struct{}

func (LinuxCommands) Document() string { return "AWS-RunShellScript" }
func (LinuxCommands) Exec(argv []string) string {
	args := make([]string, len(argv))
	for i, arg := range argv {
		args[i] = quote(arg)
	}
	return "exec " + strings.Join(args, " ")
}
func (l LinuxCommands) Pipe(url string, argv []string) string {
	args := make([]string, len(argv))
	for i, arg := range argv {
		args[i] = quote(arg)
	}
	return l.Exec([]string{"bash", "-o", "pipefail", "-c", "curl -fsS --max-time 600 " + quote(url) + " | " + strings.Join(args, " ")})
}
func (l LinuxCommands) Put(url, dst string, mode fs.FileMode) string {
	return l.Pipe(url, []string{"sh", "-c", "umask 077; mkdir -p -- \"$(dirname -- \"$1\")\" && cat > \"$1\" && chmod \"$2\" \"$1\"", "cldt-put", dst, fmt.Sprintf("%04o", mode.Perm())})
}

type WindowsCommands struct{}

func (WindowsCommands) Document() string { return "AWS-RunPowerShellScript" }
func psQuote(s string) string            { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// windowsArg follows the Microsoft C runtime argv quoting convention. Native
// invocation through PowerShell 5's & operator loses embedded argument quotes.
func windowsArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		if c == '\\' {
			slashes++
			continue
		}
		if c == '"' {
			b.WriteString(strings.Repeat("\\", slashes*2+1))
		} else {
			b.WriteString(strings.Repeat("\\", slashes))
		}
		slashes = 0
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat("\\", slashes*2))
	b.WriteByte('"')
	return b.String()
}

func windowsProcess(argv []string, input string) string {
	args := make([]string, len(argv)-1)
	for i, arg := range argv[1:] {
		args[i] = windowsArg(arg)
	}
	script := "$ErrorActionPreference = 'Stop'\n" +
		"$p = New-Object System.Diagnostics.Process\n" +
		"$p.StartInfo.FileName = " + psQuote(argv[0]) + "\n" +
		"$p.StartInfo.Arguments = " + psQuote(strings.Join(args, " ")) + "\n" +
		"$p.StartInfo.UseShellExecute = $false\n" +
		"$p.StartInfo.RedirectStandardOutput = $true\n" +
		"$p.StartInfo.RedirectStandardError = $true\n"
	if input != "" {
		script += "$p.StartInfo.RedirectStandardInput = $true\n"
	}
	script += "$null = $p.Start()\n$out = $p.StandardOutput.ReadToEndAsync()\n$err = $p.StandardError.ReadToEndAsync()\n"
	if input != "" {
		script += "$f = [System.IO.File]::OpenRead(" + input + ")\ntry { $f.CopyTo($p.StandardInput.BaseStream) } finally { $f.Dispose(); $p.StandardInput.Close() }\n"
	}
	return script + "$p.WaitForExit()\n[Console]::Out.Write($out.Result)\n[Console]::Error.Write($err.Result)\nexit $p.ExitCode\n"
}

func (WindowsCommands) Exec(argv []string) string { return windowsProcess(argv, "") }
func (WindowsCommands) Pipe(url string, argv []string) string {
	return windowsDownload(url) + "try {\n" + windowsProcess(argv, "$inputFile") + "} finally { Remove-Item -LiteralPath $inputFile -Force -ErrorAction SilentlyContinue }\n"
}

// Download into a System/Admin-only file before launching the consumer. An OS
// pipe is used for stdin so PowerShell cannot reinterpret binary input as text.
func windowsDownload(url string) string {
	return "$ErrorActionPreference = 'Stop'\n$inputFile = [System.IO.Path]::GetTempFileName()\n" +
		"& icacls.exe $inputFile /inheritance:r /grant:r '*S-1-5-18:(F)' '*S-1-5-32-544:(F)' | Out-Null\n" +
		"if ($LASTEXITCODE -ne 0) { Remove-Item -LiteralPath $inputFile -Force; throw 'Cannot restrict staged input ACL' }\n" +
		"$downloadProgressPreference = $ProgressPreference\n" +
		"try { $ProgressPreference = 'SilentlyContinue'; Invoke-WebRequest -UseBasicParsing -TimeoutSec 600 -Uri " + psQuote(url) + " -OutFile $inputFile } catch { Remove-Item -LiteralPath $inputFile -Force; throw 'Private command input download failed' } finally { $ProgressPreference = $downloadProgressPreference }\n"
}

// Windows files are restricted to SYSTEM and Administrators. POSIX mode bits
// are not translated into a broader Windows ACL.
func (WindowsCommands) Put(url, dst string, _ fs.FileMode) string {
	return windowsDownload(url) + "try {\n$destination = " + psQuote(dst) + "\n" +
		"$null = New-Item -ItemType Directory -Force -Path ([System.IO.Path]::GetDirectoryName($destination))\n" +
		"Move-Item -LiteralPath $inputFile -Destination $destination -Force\n" +
		"} finally { Remove-Item -LiteralPath $inputFile -Force -ErrorAction SilentlyContinue }\n"
}

func (LinuxCommands) BootstrapFailure(ctx context.Context, node rig.Node) error {
	return rig.CloudInitFailure(ctx, node)
}

// Only the detached secure consumer's identity-bound receipt can establish
// Windows bootstrap failure; launch-agent status and absent receipts cannot.
func (WindowsCommands) BootstrapFailure(ctx context.Context, node rig.Node) error {
	_, err := windowsBootstrapResult(ctx, node)
	return err
}

func (WindowsCommands) BootstrapComplete(ctx context.Context, node rig.Node) (bool, error) {
	return windowsBootstrapResult(ctx, node)
}

// Linux completion uses cloud-init and CAPI's success sentinel.
func (LinuxCommands) BootstrapComplete(ctx context.Context, node rig.Node) (bool, error) {
	return rig.CloudInitComplete(ctx, node)
}
