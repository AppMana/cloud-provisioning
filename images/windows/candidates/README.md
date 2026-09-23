# Prepared k0s / Calico candidates

`k0s-1.36-calico-3.32.json` is a list of candidate runtime images, not a
qualification result or a complete offline bundle. It now selects AppMana's
Calico 3.32.2 node and separate CNI installer from source
`338d8a0c46d273b8f6513007063bf7169e0d71b6`, published by
https://github.com/AppMana/forks-calico-windows-ipv6/actions/runs/35815057847.
The kube-proxy candidate identifies AppMana source
`6d89988d2cc2ddcdd78138fe827b68232c5df8ab`, Kubernetes 1.36.2.

The previous list mixed a Calico 3.32.1 node with upstream Windows CNI; do not
reuse it as evidence for the aligned fork scenario. Image digests and successful
builds establish content selection, not working Windows pod/service networking.
Preload pause and workload images separately, verify the prepared k0s binary,
and run the isolated data-path qualification before declaring the bundle ready.
