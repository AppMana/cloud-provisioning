# AWS VPC CNI upstream

`aws-vpc-cni-1.23.2.tgz` is the aws-vpc-cni Helm chart 1.23.2 from
https://aws.github.io/eks-charts (sha256
`457260209b976423671e5d1f8a6839df06000e6de516a6e6cebfb419d0f1618f`), with
images pinned in `values.yaml` to the multi-architecture digests of
`public.ecr.aws/eks/amazon-k8s-cni:v1.23.2` and
`public.ecr.aws/eks/amazon-k8s-cni-init:v1.23.2`. `go generate` (`render.sh`)
renders it to `aws-vpc-cni.yaml`; a test fails if the committed render differs.
The chart's static RBAC is shipped in the product chart
(`templates/aws-vpc-cni.yaml`); the controller applies the DaemonSet and
ConfigMap with the cluster's facts.
