#!/usr/bin/env python3
"""Prepare an isolated Linux AWS worker campaign on an existing VM site.

Run on the setup host with setup credentials loaded through source-me.sh. Each
step calls the helper that owns it (build-binaries.sh, publisher.py, assets.py);
this command orders them, gives the controller its AWS credentials as an
operator does (a Secret the chart's aws.credentials names), issues the receipt
serving certificate through the site's cert-manager and renders the chart
values from what those helpers recorded. Without --apply it stops after a
server dry run.
"""
import argparse
import hashlib
import json
import os
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
NAMESPACE = 'cloud-provisioning'
AWS_SECRET = 'cloud-provisioning-aws'
CONTROLLER = 'cloud-provisioning-endpoint-controller'
RECEIPT_SECRET = 'cldt-receipt-serving'
RECEIPT_PORT = 9444
# UDP ports already permitted by the run-owned security group's self rule.
RECEIVE_PORTS = list(range(51821, 51841))
ARCHITECTURES = ('amd64', 'arm64')
COMPONENTS = {'endpoint-controller': 'linuxController', 'dialer': 'linuxDialer'}
PUBLISHED = {'endpoint-controller': 'endpoint-controller', 'dialer': 'linux-dialer'}


def receipt_tls_objects(run_id):
    """A run-scoped CA and serving certificate; cert-manager renews both."""
    labels = {'cloud-provisioning-test': run_id}
    ca, issuer = run_id + '-receipt-ca', run_id + '-receipt-issuer'
    def meta(name):
        return {'name': name, 'namespace': NAMESPACE, 'labels': labels}
    return [
        {'apiVersion': 'cert-manager.io/v1', 'kind': 'Issuer', 'metadata': meta(run_id + '-receipt-root'),
         'spec': {'selfSigned': {}}},
        {'apiVersion': 'cert-manager.io/v1', 'kind': 'Certificate', 'metadata': meta(ca),
         'spec': {'isCA': True, 'commonName': 'receipt CA ' + run_id, 'secretName': ca,
                  'privateKey': {'algorithm': 'ECDSA', 'size': 256},
                  'issuerRef': {'name': run_id + '-receipt-root', 'kind': 'Issuer'}}},
        {'apiVersion': 'cert-manager.io/v1', 'kind': 'Issuer', 'metadata': meta(issuer),
         'spec': {'ca': {'secretName': ca}}},
        {'apiVersion': 'cert-manager.io/v1', 'kind': 'Certificate', 'metadata': meta(RECEIPT_SECRET),
         'spec': {'secretName': RECEIPT_SECRET, 'dnsNames': [f'{CONTROLLER}-receipts.{NAMESPACE}.svc'],
                  'usages': ['server auth', 'digital signature', 'key encipherment'],
                  'privateKey': {'algorithm': 'ECDSA', 'size': 256},
                  'issuerRef': {'name': issuer, 'kind': 'Issuer'}}},
    ]


def aws_secret(keys):
    """The controller's AWS identity as the chart reads it: AWS_* keys for the
    SDK's standard chain."""
    if sorted(keys) != ['AWS_ACCESS_KEY_ID', 'AWS_REGION', 'AWS_SECRET_ACCESS_KEY']:
        raise ValueError('controller credentials need exactly an access key, its secret and a region')
    return {'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
            'metadata': {'name': AWS_SECRET, 'namespace': NAMESPACE}, 'stringData': keys}


def split_image(reference):
    repository, digest = reference.split('@', 1)
    if not digest.startswith('sha256:') or len(digest) != 71:
        raise ValueError('published images must be immutable digests')
    return {'repository': repository, 'tag': '', 'digest': digest, 'pullPolicy': 'IfNotPresent'}


def render_values(state, urls, binaries, native_cidrs):
    """Chart values for the campaign, from the run inventory and asset URLs only."""
    for component in COMPONENTS.values():
        if not state.get(component + 'Image') or not state.get(component + 'PullSecret'):
            raise ValueError(component + ' has not been published for this run')
    if not native_cidrs:
        raise ValueError('receiver preparation needs every protected cloud range')
    dialer = state['linuxDialerImage']
    binary = {}
    for arch in ARCHITECTURES:
        name = 'wg-dialer-linux-' + arch
        if name not in urls or arch not in binaries:
            raise ValueError('missing first-boot dialer for ' + arch)
        binary[arch] = {'url': urls[name], 'sha256': binaries[arch]}
    return {
        'image': split_image(state['linuxControllerImage']),
        'dialerImage': split_image(dialer),
        'imagePullSecrets': [{'name': state['linuxControllerPullSecret']}, {'name': state['linuxDialerPullSecret']}],
        'dialerBinary': binary,
        'aws': {'credentials': {'secretName': AWS_SECRET, 'mount': 'env'}},
        'providerManagerNamespace': 'capa-system',
        'importedControlPlane': {'providerServiceAccounts': [{'name': 'capa-controller-manager', 'namespace': 'capa-system'}]},
        'tunnel': {
            'receiptTLSSecret': RECEIPT_SECRET,
            'receivePreparation': {
                'nativeCIDRs': list(native_cidrs),
                'ports': RECEIVE_PORTS,
                'journal': {
                    'roundLifetime': '5m', 'maxReceiptAge': '2m', 'drainInterval': '5s',
                    'expiryRecovery': 'rollback',
                    'receipts': {
                        'bindAddress': f':{RECEIPT_PORT}',
                        'certFile': '/etc/cloud-provisioning/receipts/tls.crt',
                        'keyFile': '/etc/cloud-provisioning/receipts/tls.key',
                        # The receipt policy admits the deployed dialer's index on
                        # every architecture; agents report what they run.
                        'imageIDs': {arch: dialer for arch in ARCHITECTURES},
                        'releaseObservationMaxAge': '90s',
                        'finalizeRetirements': True, 'releaseRetirements': True,
                        'finalizeOrphanRetirements': False, 'releaseOrphanRetirements': False,
                    },
                },
            },
        },
    }


def run(argv, **kw):
    print('+', ' '.join(map(str, argv[:4])), '...', flush=True)
    return subprocess.run(argv, check=True, **kw)


def bastion(runner, api_server, *argv, stdin=None):
    return run(['docker', 'exec', '-i', runner, 'docker', 'exec', '-i', 'clab-cldt-bastion', *argv],
               input=stdin, capture_output=True, text=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir', type=pathlib.Path, required=True, help='private AWS run directory')
    p.add_argument('--runner', required=True, help='isolated Docker runner containing the bastion')
    p.add_argument('--api-server', required=True)
    p.add_argument('--cluster-uid', required=True, help='observed kube-system namespace UID')
    p.add_argument('--native-cidr', action='append', required=True, help='protected cloud range; repeat')
    p.add_argument('--builder', required=True, help='docker-container buildx builder for multi-platform OCI output')
    p.add_argument('--chart', default='/tmp/chart', help='chart directory on the bastion')
    p.add_argument('--apply', action='store_true', help='upgrade the release after the server dry run')
    a = p.parse_args()
    os.umask(0o077)
    state_path = a.work_dir / 'resources.json'
    state = json.loads(state_path.read_text())
    if state.get('cleanedUp') or not state['runID'].startswith('cldt-'):
        p.error('require an active cldt run')
    revision = run(['git', '-C', str(REPO), 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
    if run(['git', '-C', str(REPO), 'status', '--porcelain'], capture_output=True, text=True).stdout.strip():
        p.error('build only a committed candidate')
    out = a.work_dir / ('candidate-' + revision[:12])
    out.mkdir(exist_ok=True)
    binaries_dir = out / 'binaries'
    run(['bash', str(REPO / 'controller/build-binaries.sh'), str(binaries_dir), 'all', *ARCHITECTURES],
        env=dict(os.environ, GOMAXPROCS='2'))
    binaries = {arch: hashlib.sha256((binaries_dir / ('wg-dialer-linux-' + arch)).read_bytes()).hexdigest()
                for arch in ARCHITECTURES}
    for target, component in PUBLISHED.items():
        layout = out / (target + '-oci')
        if not layout.exists():
            run(['docker', 'buildx', 'build', '--builder', a.builder, '--pull',
                 '--platform', ','.join('linux/' + arch for arch in ARCHITECTURES),
                 '--build-context', 'binaries=' + str(binaries_dir), '--target', target,
                 '--output', f'type=oci,dest={layout},tar=false', '-f', 'controller/Dockerfile', '.'], cwd=REPO)
        run([sys.executable, str(HERE / 'publisher.py'), '--work-dir', str(a.work_dir), '--layout', str(layout),
             '--component', component, '--api-server', a.api_server, '--runner', a.runner])
    run([sys.executable, str(HERE / 'assets.py'), '--work-dir', str(a.work_dir),
         *[arg for arch in ARCHITECTURES for arg in ('--file', str(binaries_dir / ('wg-dialer-linux-' + arch)))]])
    # Server-side, so the keys are not copied into a last-applied annotation.
    bastion(a.runner, a.api_server, 'kubectl', '--server=' + a.api_server, 'apply', '--server-side',
            '--field-manager=cldt-operator', '-f', '-', stdin=json.dumps(aws_secret(json.loads((a.work_dir / 'controller-credentials.json').read_text()))))
    tls = {'apiVersion': 'v1', 'kind': 'List', 'items': receipt_tls_objects(state['runID'])}
    bastion(a.runner, a.api_server, 'kubectl', '--server=' + a.api_server, 'apply', '-f', '-', stdin=json.dumps(tls))
    bastion(a.runner, a.api_server, 'kubectl', '--server=' + a.api_server, '-n', NAMESPACE, 'wait',
            '--for=condition=Ready', '--timeout=2m', 'certificate/' + RECEIPT_SECRET)
    state = json.loads(state_path.read_text())
    urls = json.loads((a.work_dir / 'asset-urls.json').read_text())
    values = render_values(state, urls, binaries, a.native_cidr)
    values_path = out / 'campaign-values.json'
    values_path.write_text(json.dumps(values, indent=2) + '\n')
    helm = ['helm', 'upgrade', 'cloud-provisioning', a.chart, '-n', NAMESPACE, '--reuse-values', '-f', '-']
    bastion(a.runner, a.api_server, *helm, '--dry-run=server', stdin=json.dumps(values))
    if a.apply:
        bastion(a.runner, a.api_server, *helm, '--wait', '--timeout', '10m', stdin=json.dumps(values))
    print(('Applied' if a.apply else 'Server dry run passed for') + ' campaign values for ' + revision[:12]
          + '; values saved privately in ' + str(values_path))


if __name__ == '__main__':
    main()
