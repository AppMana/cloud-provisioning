#!/usr/bin/env python3
"""Publish only the derived CAPA session through the explicitly named bastion."""
import argparse,base64,json,pathlib,subprocess
p=argparse.ArgumentParser()
p.add_argument('--work-dir',required=True)
p.add_argument('--api-server',required=True)
p.add_argument('--bastion',default='clab-cldt-bastion')
a=p.parse_args()
work=pathlib.Path(a.work_dir);s=json.loads((work/'resources.json').read_text())
if s.get('cleanedUp'):raise RuntimeError('run has been cleaned up')
c=json.loads((work/'capa-session.json').read_text())['Credentials']
def apply(o):
 r=subprocess.run(['docker','exec','-i',a.bastion,'kubectl','--server='+a.api_server,'apply','-f','-'],input=json.dumps(o),capture_output=True,text=True)
 if r.returncode:raise RuntimeError('derived CAPA identity publication failed')
apply({'apiVersion':'v1','kind':'Secret','metadata':{'name':s['runID'],'namespace':'capa-system'},'type':'Opaque','data':{k:base64.b64encode(c[v].encode()).decode() for k,v in {'AccessKeyID':'AccessKeyId','SecretAccessKey':'SecretAccessKey','SessionToken':'SessionToken'}.items()}})
apply({'apiVersion':'infrastructure.cluster.x-k8s.io/v1beta2','kind':'AWSClusterStaticIdentity','metadata':{'name':s['runID']},'spec':{'secretRef':s['runID'],'allowedNamespaces':{'list':['cloud-provisioning']}}})
print('Published derived CAPA identity',s['runID'])
