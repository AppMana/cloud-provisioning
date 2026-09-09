#!/usr/bin/env python3
"""Renew an existing Linux dialer S3 URL without changing its configured binary.

Run with setup credentials from source-me.sh. A separate exact-object session
signs the URL; source credentials never enter Kubernetes. Evidence is private.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import subprocess
import urllib.parse


def artifact(deployment, state, architecture):
    prefix='--join-dialer-binary-url-'+architecture+'='
    checksum='--join-dialer-binary-sha256-'+architecture+'='
    matches=[]
    for ci,container in enumerate(deployment['spec']['template']['spec']['containers']):
        args=container.get('args',[])
        for ai,arg in enumerate(args):
            if arg.startswith(prefix):
                hashes=[a[len(checksum):] for a in args if a.startswith(checksum)]
                if len(hashes)!=1 or not re.fullmatch('[0-9a-f]{64}',hashes[0]):
                    raise ValueError('exact configured artifact checksum required')
                url=urllib.parse.urlparse(arg[len(prefix):]);bucket=state['assetBucket']
                hosts=[bucket+'.s3.'+state['region']+'.amazonaws.com',bucket+'.s3.amazonaws.com']
                key=urllib.parse.unquote(url.path.lstrip('/'))
                if (url.scheme!='https' or url.hostname not in hosts or url.username or
                        url.password or url.port or not key.startswith('downloads/') or
                        not key.removeprefix('downloads/')):
                    raise ValueError('artifact must be in this run private S3 downloads prefix')
                matches.append(dict(container=ci,argument=ai,prefix=prefix,bucket=bucket,key=key,sha256=hashes[0]))
    if len(matches)!=1:raise ValueError('exactly one configured artifact URL required')
    return matches[0]


def url_patch(deployment, selected, url):
    return [{'op':'test','path':'/metadata/resourceVersion','value':deployment['metadata']['resourceVersion']},
            {'op':'replace','path':f"/spec/template/spec/containers/{selected['container']}/args/{selected['argument']}",
             'value':selected['prefix']+url}]


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir',type=pathlib.Path,required=True)
    p.add_argument('--evidence-dir',type=pathlib.Path,required=True)
    p.add_argument('--api-server',required=True)
    p.add_argument('--bastion',default='clab-cldt-bastion')
    p.add_argument('--architecture',choices=['amd64','arm64'],default='amd64')
    a=p.parse_args();os.umask(0o077)
    a.evidence_dir.mkdir(mode=0o700)
    state=json.loads((a.work_dir/'resources.json').read_text())
    if state.get('cleanedUp'):raise ValueError('run is cleaned up')
    def aws(args,env=None):
        out=subprocess.run(['aws','--region',state['region']]+args,capture_output=True,text=True,env=env,timeout=120)
        if out.returncode:raise RuntimeError('AWS artifact renewal failed; no credentials printed')
        return out.stdout
    if json.loads(aws(['sts','get-caller-identity']))['Account']!=state['account']:
        raise ValueError('AWS account mismatch')
    kube=['docker','exec','-i',a.bastion,'kubectl','--server='+a.api_server,'-n','cloud-provisioning']
    raw=subprocess.run(kube+['get','deployment','cloud-provisioning-endpoint-controller','-o','json'],capture_output=True,text=True,check=True,timeout=30)
    deployment=json.loads(raw.stdout);selected=artifact(deployment,state,a.architecture)
    target=a.evidence_dir/'verified-binary'
    aws(['s3api','get-object','--bucket',selected['bucket'],'--key',selected['key'],str(target)])
    if hashlib.sha256(target.read_bytes()).hexdigest()!=selected['sha256']:
        raise ValueError('existing S3 artifact does not match configured checksum')
    resource='arn:aws:s3:::'+selected['bucket']+'/'+selected['key']
    policy={'Version':'2012-10-17','Statement':[{'Effect':'Allow','Action':'s3:GetObject','Resource':resource}]}
    session=json.loads(aws(['sts','get-federation-token','--name','cldt-assets','--duration-seconds','14400','--policy',json.dumps(policy)]))['Credentials']
    env=dict(os.environ,AWS_ACCESS_KEY_ID=session['AccessKeyId'],AWS_SECRET_ACCESS_KEY=session['SecretAccessKey'],AWS_SESSION_TOKEN=session['SessionToken'])
    url=aws(['s3','presign','s3://'+selected['bucket']+'/'+selected['key'],'--expires-in','14400'],env).strip()
    patch=url_patch(deployment,selected,url)
    (a.evidence_dir/'private-patch.json').write_text(json.dumps(patch))
    changed=subprocess.run(kube+['patch','deployment','cloud-provisioning-endpoint-controller','--type=json','--patch-file','/dev/stdin'],input=json.dumps(patch),capture_output=True,text=True,timeout=30)
    if changed.returncode:raise RuntimeError('conditional deployment update failed; inspect current resource version')
    report=dict(sha256=selected['sha256'],sameObjectVerified=True,updatedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),expiresAt=session['Expiration'])
    (a.evidence_dir/'result.json').write_text(json.dumps(report,indent=2)+'\n')
    print('Renewed existing artifact URL; checksum verified and Deployment update conditional on resource version')

if __name__=='__main__':main()
