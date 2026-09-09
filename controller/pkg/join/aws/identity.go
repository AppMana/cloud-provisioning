package aws

import "encoding/base64"

// BootstrapIdentityScript discovers the immutable EC2 identity after launch.
// CAPA cannot report an instance ID before its bootstrap Secret exists. The
// distribution consumes the resulting kubelet flag without knowing AWS APIs.
const BootstrapIdentityScript = `#!/bin/sh
set -eu
token=$(curl -fsS --connect-timeout 2 --max-time 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
id=$(curl -fsS --connect-timeout 2 --max-time 5 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/instance-id)
az=$(curl -fsS --connect-timeout 2 --max-time 5 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/placement/availability-zone)
case "$id" in i-*) ;; *) exit 1 ;; esac
case "$id$az" in *[!a-z0-9-]*|'') exit 1 ;; esac
[ -n "$az" ]
printf 'aws:///%s/%s\n' "$az" "$id"
`

func bootstrapIdentityValues() map[string]any {
	return map[string]any{"providerIdentityScript": base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityScript)), "providerIdentityPowerShell": base64.StdEncoding.EncodeToString([]byte(BootstrapIdentityPowerShell))}
}

// BootstrapIdentityPowerShell is the native Windows implementation of the
// same EC2 identity contract. IMDSv2 requests are bounded and fail closed.
const BootstrapIdentityPowerShell = `$ErrorActionPreference='Stop'
$base='http://169.254.169.254/latest'
$token=Invoke-RestMethod -Method Put -Uri "$base/api/token" -Headers @{'X-aws-ec2-metadata-token-ttl-seconds'='60'} -TimeoutSec 5
$headers=@{'X-aws-ec2-metadata-token'=$token}
$id=Invoke-RestMethod -Uri "$base/meta-data/instance-id" -Headers $headers -TimeoutSec 5
$az=Invoke-RestMethod -Uri "$base/meta-data/placement/availability-zone" -Headers $headers -TimeoutSec 5
$ip=Invoke-RestMethod -Uri "$base/meta-data/local-ipv4" -Headers $headers -TimeoutSec 5
if ($id -notmatch '^i-[a-f0-9]+$' -or $az -notmatch '^[a-z0-9-]+$') { throw 'Invalid EC2 identity' }
$parsed=[Net.IPAddress]::Parse($ip)
if ($parsed.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { throw 'Expected primary IPv4 address' }
@{providerID="aws:///$az/$id";nodeAddress=$parsed.ToString()}
`
