#!/usr/bin/env python3
"""Render AWS VPC CNI Helm values for one isolated Linux pod-family campaign."""
import argparse
import hashlib
import ipaddress
import json
import re
from pathlib import Path
import subprocess
import tempfile

CHART_VERSION = '1.23.2'
CHART_SHA256 = '457260209b976423671e5d1f8a6839df06000e6de516a6e6cebfb419d0f1618f'


def values(family, site_cidrs, service_cidrs, vpc_cidrs, run_id, image, init_image, mtu=1420):
    if family not in ('ipv4', 'ipv6'):
        raise ValueError('choose exactly one AWS pod family')
    if not re.fullmatch(r'cldt-[a-zA-Z0-9-]+', run_id):
        raise ValueError('require an isolated test run ID')
    for reference in (image, init_image):
        if not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', reference):
            raise ValueError('CNI and init images must use immutable content digests')
    if not 1280 <= mtu <= 1420:
        raise ValueError('pod MTU must fit the qualified 1420-byte transport and IPv6 minimum')
    groups = [[ipaddress.ip_network(c, strict=True) for c in group]
              for group in (site_cidrs, service_cidrs, vpc_cidrs)]
    if not all(groups):
        raise ValueError('site, service and VPC ranges are all required')
    for i, group in enumerate(groups):
        if len(set(group)) != len(group):
            raise ValueError('duplicate network range')
        for left in group:
            if left.prefixlen == 0 or left.is_multicast or left.is_loopback or left.is_link_local:
                raise ValueError('invalid workload or service range')
            for other in groups[i+1:]:
                if any(left.version == right.version and left.overlaps(right) for right in other):
                    raise ValueError('site, service and VPC ranges must not overlap')
    version = 4 if family == 'ipv4' else 6
    if not all(any(n.version == version for n in group) for group in groups):
        raise ValueError('site, service and VPC ranges must contain the requested pod family')
    v6 = family == 'ipv6'
    env = {
        'ENABLE_IPv4': str(not v6).lower(), 'ENABLE_IPv6': str(v6).lower(),
        'ENABLE_PREFIX_DELEGATION': str(v6).lower(),
        'AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG': 'false',
        'AWS_VPC_K8S_CNI_EXTERNALSNAT': 'false',
        # Preserve site-bound IPv4 sources while retaining ordinary VPC egress SNAT.
        'AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS': ','.join(str(n) for n in groups[0] + groups[1] if n.version == 4),
        'AWS_VPC_ENI_MTU': '1500', 'POD_MTU': str(mtu),
        'ENABLE_POD_ENI': 'false', 'ENABLE_MULTI_NIC': 'false',
        'ENABLE_SUBNET_DISCOVERY': 'false',
        'ADDITIONAL_ENI_TAGS': json.dumps({'cloud-provisioning-test': run_id}, separators=(',', ':')),
        'MAX_ENI': '1', 'WARM_ENI_TARGET': '0',
    }
    if v6:
        env['WARM_PREFIX_TARGET'] = '1'
    else:
        env.update(WARM_IP_TARGET='1', MINIMUM_IP_TARGET='2')
    return {
        'nodeSelector': {'kubernetes.io/os': 'linux', 'cloud-provisioning.appmana.com/cni': 'aws-vpc',
                         'cloud-provisioning.appmana.com/role': 'cloud-worker'},
        'enableNetworkPolicy': 'false', 'nodeAgent': {'enabled': False},
        'image': {'override': image},
        'init': {'image': {'override': init_image}, 'env': {'ENABLE_IPv6': str(v6).lower()}},
        'env': env,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--chart', type=Path, required=True, help='pinned aws-vpc-cni chart archive')
    parser.add_argument('--family', choices=('ipv4', 'ipv6'), required=True)
    for name in ('site-cidr', 'service-cidr', 'vpc-cidr'):
        parser.add_argument('--' + name, action='append', required=True)
    for name in ('run-id', 'image', 'init-image'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--pod-mtu', type=int, default=1420)
    args = parser.parse_args()
    try:
        rendered = values(args.family, args.site_cidr, args.service_cidr, args.vpc_cidr,
                          args.run_id, args.image, args.init_image, args.pod_mtu)
    except ValueError as error:
        parser.error(str(error))
    if hashlib.sha256(args.chart.read_bytes()).hexdigest() != CHART_SHA256:
        parser.error('chart archive digest does not match the pinned release')
    with tempfile.TemporaryDirectory(prefix='vpc-cni-render-') as directory:
        path = Path(directory) / 'values.json'
        path.write_text(json.dumps(rendered))
        result = subprocess.run(['helm', 'template', 'aws-vpc-cni', str(args.chart.resolve()),
                                 '--namespace', 'kube-system', '--values', str(path)],
                                check=True, capture_output=True, text=True)
        print(result.stdout, end='')


if __name__ == '__main__':
    main()
