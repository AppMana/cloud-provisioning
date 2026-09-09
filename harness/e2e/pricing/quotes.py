#!/usr/bin/env python3
"""Read-only GPU VM quotes. Catalog prices are not capacity or support proofs."""
import argparse
import datetime as dt
import decimal
import json
import math
import pathlib
import subprocess
import urllib.request

D = decimal.Decimal


def aws(region, operation, **inputs):
    result = subprocess.run(['aws', '--region', region, 'ec2', operation,
                             '--cli-input-json', json.dumps(inputs), '--output', 'json'],
                            capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError(f'EC2 {operation} failed in {region}')
    return json.loads(result.stdout)


def aws_rows(region, types, now, call=aws):
    machines = call(region, 'describe-instance-types', Filters=[{'Name': 'instance-type', 'Values': types}])['InstanceTypes']
    machines = {m['InstanceType']: m for m in machines if m.get('GpuInfo', {}).get('Gpus')}
    if not machines:
        return []
    history = call(region, 'describe-spot-price-history', InstanceTypes=list(machines),
                   ProductDescriptions=['Linux/UNIX', 'Windows'], StartTime=now, EndTime=now)['SpotPriceHistory']
    latest = {}
    for price in history:
        key = (price['InstanceType'], price['AvailabilityZone'], price['ProductDescription'])
        if key not in latest or price['Timestamp'] > latest[key]['Timestamp']:
            latest[key] = price
    rows = []
    for (kind, zone, product), price in latest.items():
        machine = machines[kind]
        gpus = machine['GpuInfo']['Gpus']
        rows.append(dict(provider='aws', market='spot', region=region, zone=zone,
                         machine=kind, os='windows' if product == 'Windows' else 'linux',
                         hourlyUSD=price['SpotPrice'], minimumSeconds=60, billingIncrementSeconds=1,
                         vcpus=machine['VCpuInfo']['DefaultVCpus'], memoryMiB=machine['MemoryInfo']['SizeInMiB'],
                         gpu=gpus, gpuMemoryMiB=machine['GpuInfo']['TotalGpuMemoryInMiB'],
                         logicalGPUs=sum(g.get('LogicalGpuCount', g['Count']) for g in gpus),
                         windowsLicenseIncluded=product == 'Windows',
                         excludedCosts=['EBS', 'public IPv4', 'egress', 'snapshots', 'image builds', 'retries'],
                         capacityVerified=False, imageValidated=False, effectiveAt=price['Timestamp']))
    return rows


def vultr_rows(plans):
    rows = []
    for plan in plans:
        # VCG API explicitly identifies GPU VM plans. Do not infer VM support
        # for providers whose catalog contains only container rental offers.
        if not plan['id'].startswith('vcg-'):
            continue
        if not plan.get('deploy_ondemand', False):
            continue
        if plan.get('location_cost'):
            raise ValueError('regional Vultr prices require an explicit schema adapter')
        for region in plan.get('locations', []):
            for guest in ('linux', 'windows'):
                rows.append(dict(provider='vultr', market='on-demand', region=region,
                                 machine=plan['id'], os=guest, hourlyUSD=str(plan['hourly_cost']),
                                 minimumSeconds=3600, billingIncrementSeconds=3600,
                                 vcpus=plan['vcpu_count'], memoryMiB=plan['ram'],
                                 diskGB=plan['disk'], gpu=plan.get('gpu_type'),
                                 gpuMemoryMiB=plan['gpu_vram_gb'] * 1024,
                                 gpuFraction=plan['gpu_count'],
                                 windowsLicenseIncluded=False,
                                 excludedCosts=(['Windows license'] if guest == 'windows' else []) +
                                               ['egress over allowance', 'snapshots', 'image builds', 'retries'],
                                 capacityVerified=False, imageValidated=False))
    return rows


def cost(row, seconds):
    if not math.isfinite(seconds) or seconds <= 0:
        raise ValueError('session duration must be finite and positive')
    increment = row['billingIncrementSeconds']
    billed = max(row['minimumSeconds'], math.ceil(seconds / increment) * increment)
    rate = D(row['hourlyUSD'])
    if not rate.is_finite() or rate < 0:
        raise ValueError('invalid hourly quote')
    return rate * D(billed) / D(3600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--regions', default='us-west-2,us-east-1,eu-north-1,eu-west-1')
    parser.add_argument('--instance-types', default='g6f.large,g6f.xlarge,g4dn.xlarge,g5.xlarge')
    parser.add_argument('--providers', default='aws,vultr')
    parser.add_argument('--minutes', type=float, default=20, help='total billed lifecycle, including startup and cleanup')
    parser.add_argument('--output', required=True, type=pathlib.Path)
    args = parser.parse_args()
    if not math.isfinite(args.minutes) or args.minutes <= 0:
        parser.error('minutes must be finite and positive')
    providers = args.providers.split(',')
    if set(providers) - {'aws', 'vultr'}:
        parser.error('supported collectors: aws,vultr')
    now = dt.datetime.now(dt.timezone.utc).isoformat()
    rows, errors = [], []
    if 'aws' in providers:
        for region in args.regions.split(','):
            try:
                rows.extend(aws_rows(region, args.instance_types.split(','), now))
            except Exception as exc:
                errors.append(dict(provider='aws', region=region, error=str(exc)))
    if 'vultr' in providers:
        try:
            url = 'https://api.vultr.com/v2/plans?type=vcg&per_page=500'
            while url:
                with urllib.request.urlopen(url, timeout=30) as response:
                    body = json.load(response)
                rows.extend(vultr_rows(body['plans']))
                url = body.get('meta', {}).get('links', {}).get('next', '')
                if url and not url.startswith('https://api.vultr.com/v2/plans?'):
                    raise ValueError('unexpected catalog pagination URL')
        except Exception as exc:
            errors.append(dict(provider='vultr', error=str(exc)))
    for row in rows:
        row['sessionBaseUSD'] = str(cost(row, args.minutes * 60))
        row['costScope'] = 'base compute; excluded costs must be added before provider selection'
    rows.sort(key=lambda r: D(r['sessionBaseUSD']))
    report = dict(schemaVersion=1, observedAt=now, lifecycleMinutes=args.minutes,
                  requestedProviders=providers, requestedAWSRegions=args.regions.split(','),
                  requestedInstanceTypes=args.instance_types.split(','), errors=errors, quotes=rows)
    # Preserve observations from previous queries, including partial failures.
    with args.output.open('x') as output:
        json.dump(report, output, indent=2)
        output.write('\n')
    print(f'{len(rows)} quotes, {len(errors)} collector failures; saved {args.output}')
    return 1 if errors or not rows else 0


if __name__ == '__main__':
    raise SystemExit(main())
