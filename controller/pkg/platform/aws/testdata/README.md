# Qualified AWS VPC CNI render

`qualified-ipv4-20261014.yaml` is the aws-node render the 2026-10-14 IPv4
native-CNI qualification ran (harness `aws/vpc_cni.py`, chart 1.23.2, run
`cldt-native-vpc4-20261014`): site node range 10.101.0.0/24, site pod range
10.101.128.0/17, Service range 10.101.4.0/22, pod MTU 1420. The product's own
render must reproduce it, apart from the run tag on ENIs.
