# Site CNI DaemonSets

`k0s-v1.36.5-calico-node.yaml` is k0s's bundled Calico node DaemonSet,
`static/manifests/calico/DaemonSet/calico-node.yaml` at k0s
v1.36.5-0.20260918181138-c37fe960bdee (Calico v3.32.1), rendered with
`text/template` the way k0s renders it, for the dual-stack BGP profile:
`mode: bird`, `overlay: Never`, MTU 1450, IPv4 and IPv6 enabled,
`ipAutodetectionMethod: cidr=10.10.0.0/24`,
`ipV6AutodetectionMethod: cidr=fd00:10::/64`, pod CIDRs 10.244.0.0/16 and
fd00:244::/56, and no extra `envVars`. Blank lines were removed.
`render-k0s-calico-node.go` regenerates it (command in its header).

`k0s-v1.36.5-calico-node-windows.yaml` is the same k0s revision's
`static/manifests/calico/DaemonSet/calico-node-windows.yaml`, which k0s renders
only in `mode: vxlan` with Windows included, rendered with the same values
plus the Windows images, API server 10.10.0.10:6443, cluster DNS 10.96.0.10
and Service CIDR 10.96.0.0/12. k0s sets its `IP` to the autodetection
method, so it detects the address at every check.
