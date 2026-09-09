set -eu
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y unzip
mkdir -p /tmp/cldt-awscli
cd /tmp/cldt-awscli
curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64-2.36.33.zip -o awscli.zip
printf '%s  %s\n' d1d0c36d2582925e52022d8675e7fdaed8bb871af97ce2ff7c69e8bd81b123a0 awscli.zip | sha256sum -c -
unzip -q awscli.zip
./aws/install --update
/usr/local/bin/aws --version
cd /
rm -rf /tmp/cldt-awscli
# Match kubernetes-sigs/image-builder 96bd49f71859ff7863f08ba4dc3321c560f8802b,
# images/capi/ansible/roles/providers/tasks/main.yml. CAPA's boothook creates
# the include file after initial parsing; strict include failure prevents it.
python3 - <<'PYCODE'
from pathlib import Path
path = Path('/usr/lib/python3/dist-packages/cloudinit/features.py')
marker = '# cloud-provisioning CAPA image prerequisite'
text = path.read_text()
if marker not in text:
    path.write_text(text + '\n' + marker + '\nERROR_ON_USER_DATA_FAILURE = False\n')
PYCODE
python3 -c 'from cloudinit import features; assert not features.ERROR_ON_USER_DATA_FAILURE'
# CAPA restarts cloud-init from its boothook. With cloud-init 26.1, killing
# the unit's tee subprocess can raise BrokenPipeError in the SIGTERM handler
# before sys.exit, allowing consume_data to be marked complete prematurely.
# Native file redirection keeps the log descriptor valid during that restart.
cat > /etc/cloud/cloud.cfg.d/99-capa-output.cfg <<'CLOUDCONFIG'
output: {all: '>> /var/log/cloud-init-output.log'}
CLOUDCONFIG
cloud-init clean --logs --machine-id --seed
sync
