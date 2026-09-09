$ErrorActionPreference='Stop'
$root='C:\gpu-probe-out'
New-Item -ItemType Directory -Force $root | Out-Null
# ConfigMap files use projected links. Windows can read the binary through that
# mount but CreateProcess may not resolve it; execute a verified local copy.
Copy-Item -LiteralPath 'C:\gpu-probe\render.exe' -Destination "$root\render.exe"
if ((Get-FileHash -LiteralPath 'C:\gpu-probe\render.exe').Hash -ne (Get-FileHash -LiteralPath "$root\render.exe").Hash) { throw 'Probe copy hash mismatch' }
$render=& "$root\render.exe" "$root\frames.bgra"
if ($LASTEXITCODE -ne 0) { throw 'Hardware rasterization failed' }
$receipt=$render | ConvertFrom-Json
if (!$receipt.hardwareOnly -or $receipt.vendorID -ne 4318 -or $receipt.frames -ne 30 -or $receipt.pixelsPerFrame -ne 4096) { throw 'Unexpected render receipt' }
if ((Get-Item "$root\frames.bgra").Length -ne (30*64*64*4)) { throw 'Raw frame size mismatch' }
# GRID 582.53 on the tested L4 rejects a 64x64 NVENC input. Preserve the
# checked rasterized content while scaling it to a supported encoder size.
$encode=Start-Process -FilePath C:\ffmpeg.exe -ArgumentList @('-hide_banner','-y','-f','rawvideo','-pixel_format','bgra','-video_size','64x64','-framerate','30','-i',"$root\frames.bgra",'-vf','scale=256:256:flags=neighbor','-c:v','h264_nvenc','-frames:v','30',"$root\encoded.mp4") -Wait -PassThru -NoNewWindow -RedirectStandardOutput "$root\encode.stdout" -RedirectStandardError "$root\encode.stderr"
if ($encode.ExitCode -ne 0) { Get-Content "$root\encode.stderr"; throw 'NVENC encoding failed' }
$decode=Start-Process -FilePath C:\ffmpeg.exe -ArgumentList @('-hide_banner','-i',"$root\encoded.mp4",'-progress',"$root\decode.progress",'-f','null','-') -Wait -PassThru -NoNewWindow -RedirectStandardOutput "$root\decode.stdout" -RedirectStandardError "$root\decode.stderr"
if ($decode.ExitCode -ne 0) { Get-Content "$root\decode.stderr"; throw 'Encoded frame decode failed' }
$progress=Get-Content "$root\decode.progress"
$frames=@($progress | Where-Object { $_ -match '^frame=\d+$' })
if (!$frames.Count -or $frames[-1] -ne 'frame=30' -or $progress[-1] -ne 'progress=end') { throw 'Expected 30 fully decoded frames' }
@{render=$receipt;encoder='h264_nvenc';decodedFrames=30;encodedBytes=(Get-Item "$root\encoded.mp4").Length;containerWindowsBuild=[Environment]::OSVersion.Version.Build} | ConvertTo-Json -Depth 5 -Compress
