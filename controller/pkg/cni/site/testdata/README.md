# Site CNI DaemonSets

`k0s-v1.36.5-calico-node.yaml` is k0s's bundled Calico node DaemonSet,
`static/manifests/calico/DaemonSet/calico-node.yaml` at k0s
v1.36.5-0.20260918181138-c37fe960bdee (Calico v3.32.1), rendered with
`text/template` the way k0s renders it, for the dual-stack BGP profile:
`mode: bird`, `overlay: Never`, MTU 1450, IPv4 and IPv6 enabled,
`ipAutodetectionMethod: cidr=10.10.0.0/24`,
`ipV6AutodetectionMethod: cidr=fd00:10::/64`, pod CIDRs 10.244.0.0/16 and
fd00:244::/56, and no extra `envVars`. Blank lines were removed.
