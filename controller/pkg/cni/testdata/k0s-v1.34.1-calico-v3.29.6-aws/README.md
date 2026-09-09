# k0s bundled Calico on AWS

Sanitized IPPool and BlockAffinity resources from the passing two-worker AWS
matrix at 2026-09-06T08:17:47Z: k0s v1.34.1+k0s.0, Calico v3.29.6-0, CAPA v2.12.1.
The site contains five single-NIC QEMU guests; both EC2 workers have one ENI.
Names and networking specifications are preserved; server metadata is removed.

These fixtures test CNI detection and route-prefix interpretation. Packet
reachability is established separately by the live 140-check matrix.
