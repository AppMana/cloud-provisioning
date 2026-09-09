#!/bin/sh
set -eu
command -v snap
python3 --version
dpkg-query -W -f='${Status}\n' snapd | grep -qx 'install ok installed'
test ! -d /var/snap/microk8s
test ! -e /etc/microk8s.yaml
test ! -e /etc/kubernetes/kubelet.conf
