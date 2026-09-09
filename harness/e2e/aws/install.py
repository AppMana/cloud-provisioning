#!/usr/bin/env python3
"""Install pinned CAPA for an externally managed site; no source AWS credentials."""
import argparse,hashlib,pathlib,re,subprocess,urllib.request
p=argparse.ArgumentParser()
p.add_argument('--work-dir',required=True)
p.add_argument('--api-server',required=True)
p.add_argument('--bastion',default='clab-cldt-bastion')
a=p.parse_args()
work=pathlib.Path(a.work_dir);work.mkdir(parents=True,exist_ok=True)
path=work/'infrastructure-components.yaml'
url='https://github.com/kubernetes-sigs/cluster-api-provider-aws/releases/download/v2.12.1/infrastructure-components.yaml'
if not path.exists():
 with urllib.request.urlopen(url,timeout=60) as response:path.write_bytes(response.read())
raw=path.read_bytes()
if hashlib.sha256(raw).hexdigest()!='3ab4c854f282938df9a3faae180246781bcc41ed7b4cde63baf785e6a1ee8175':
 raise RuntimeError('CAPA release manifest digest mismatch')
values={'CAPA_EKS':'false','AUTO_CONTROLLER_IDENTITY_CREATOR':'false','TAG_UNMANAGED_NETWORK_RESOURCES':'false','AWS_B64ENCODED_CREDENTIALS':'""','AWS_CONTROLLER_IAM_ROLE':'""','AWS_CONTROLLER_IAM_ROLE/#arn/eks.amazonaws.com/role-arn: arn':''}
def expand(match):
 key,sep,default=match.group(1).partition(':=')
 if key in values:return values[key]
 if sep:return default
 raise RuntimeError('unsupported CAPA variable: '+key)
manifest=re.sub(r'\$\{([^}]+)\}',expand,raw.decode())
(work/'capa-rendered.yaml').write_text(manifest)
command=['docker','exec','-i',a.bastion,'kubectl','--server='+a.api_server]
r=subprocess.run(command+['apply','-f','-'],input=manifest,capture_output=True,text=True)
(work/'capa-apply.log').write_text(r.stdout+r.stderr)
if r.returncode:raise RuntimeError('CAPA apply failed; inspect capa-apply.log')
r=subprocess.run(command+['rollout','status','deployment/capa-controller-manager','-n','capa-system','--timeout=5m'],capture_output=True,text=True)
(work/'capa-rollout.log').write_text(r.stdout+r.stderr)
if r.returncode:raise RuntimeError('CAPA rollout failed; inspect capa-rollout.log')
print('CAPA v2.12.1 is Ready for external infrastructure and static identities')
