# Real AWS bring-up

Runs the actual `wg-pullup.sh` (identical file `harness/containernet/`
drives in simulation) against one real, billed EC2 instance and a real
on-prem host, over the real internet. This is the level of fidelity
neither the raw-netns nor the containernet harness can reach: whether a
real cloud security group and a real WireGuard handshake actually work
end to end, with no NAT/firewall along the real path silently eating
outbound UDP 51820.

## One-time setup: a scoped IAM identity

Never run this against an admin credential. `../../scripts/aws/bootstrap-harness-iam.sh`
mints a narrowly-scoped IAM user (`cloud-provisioning-harness`) whose
policy only allows: describing EC2 resources, and creating/mutating
instances, security groups, and key pairs that carry
`Project=cloud-provisioning-harness` — nothing else in the account is
reachable from this identity. Run it once, under admin credentials:

```bash
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_DEFAULT_REGION=...
../../scripts/aws/bootstrap-harness-iam.sh   # prints an access key -- save it, don't commit it
```

Re-running it is safe (idempotent): it updates the policy in place and
mints a new access key without touching the old one.

## Running the harness

```bash
export AWS_ACCESS_KEY_ID=...        # the scoped harness user's key, not admin
export AWS_SECRET_ACCESS_KEY=...
export AWS_DEFAULT_REGION=us-west-2
export VPC_ID=vpc-xxxx              # any VPC with a public subnet
export SUBNET_ID=subnet-xxxx        # a subnet with MapPublicIpOnLaunch=true
export AMI_ID=ami-xxxx              # current arm64 Ubuntu 24.04, e.g.:
  # aws ec2 describe-images --owners 099720109477 \
  #   --filters "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" \
  #   --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text
export ONPREM_SSH_HOST=administrator@100.x.x.x   # the real on-prem dialer host
export ONPREM_PUBLIC_IP=203.0.113.5              # that host's real WAN egress IP

./bringup.sh    # launches, dials, verifies -- ~2-3 minutes end to end
./teardown.sh   # terminates the instance, deletes the SG and key pair,
                # and removes wg0/the test dummy interface from the on-prem host
```

`teardown.sh` is always safe to re-run; every step tolerates resources
that are already gone. Run it even if `bringup.sh` fails partway —
state (`instance_id`, `sg_id`, `key_name`) lives in `.state/`, which
`.gitignore` excludes from the repo along with any generated keys.

## What this test uses, and doesn't

- Real WireGuard keypairs, generated fresh per run.
- A disjoint test-only address range (`10.100.0.0/24` for the tunnel,
  `10.222.0.0/16` standing in for pod traffic) — chosen specifically to
  never collide with a real cluster's actual pod/VIP CIDR, so this never
  touches production Calico state on the on-prem host.
- The real security group only opens UDP 51820 and TCP 22 to
  `ONPREM_PUBLIC_IP`, nothing else, nothing world-reachable.
- No CAPA, no k0smotron, no k0s join — this proves the tunnel mechanism
  in isolation, not the full provisioning flow (that's what
  `manifests/` templates for; nothing here launches or wires up CAPI).

## Findings this test surfaced that no simulation could

- **AppArmor**: Ubuntu's shipped profile for `/usr/bin/wg`
  (`/etc/apparmor.d/usr.bin.wg`) confines it to `file rw
  @{etc_rw}/wireguard/{,**}` — a private-key file anywhere else (e.g.
  the default `mktemp` location, `/tmp`) fails with a bare `fopen:
  Permission denied` from `wg` itself, not a filesystem permission
  error. Fixed once, in `wg-pullup.sh`, so every environment (this one,
  containernet, and any real deployment) gets it.
- **`ping -I` device-vs-address binding**: `-I <device-name>` binds the
  socket to that device (`SO_BINDTODEVICE`), forcing egress out it
  literally — fatal for a dummy interface with no real link, since
  nothing ever gets routed onto `wg0` at all. `-I <address>` binds only
  the source address and leaves routing to the kernel's normal table
  lookup, which is what actually exercises the tunnel. This was a bug
  in the harness itself, not the design.
- Confirms raw `wg set` (unlike `wg-quick`) installs no kernel routes
  for `allowed-ips` — both sides need an explicit `ip route` (BGP
  supplies this in the real design; this harness adds it directly).
