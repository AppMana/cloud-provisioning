#!/usr/bin/env python3
"""Renew private, short-lived CAPA and harness sessions using setup credentials."""
import argparse,json,os,pathlib,subprocess,tempfile
p=argparse.ArgumentParser()
p.add_argument('--work-dir',required=True)
a=p.parse_args()
os.umask(0o077)
def private_write(path, body):
 fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name+'.')
 try:
  with os.fdopen(fd, 'w') as f: f.write(body)
  os.replace(tmp, path)
 finally:
  if os.path.exists(tmp): os.unlink(tmp)
work=pathlib.Path(a.work_dir)
path=work/'resources.json'
s=json.loads(path.read_text())
def aws(service,operation,**inputs):
 r=subprocess.run(['aws','--region',s['region'],service,operation,'--cli-input-json',json.dumps(inputs),'--output','json'],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(service+'/'+operation+' failed')
 return json.loads(r.stdout)
if s.get('cleanedUp') or aws('sts','get-caller-identity')['Account']!=s['account']:raise RuntimeError('inactive run or AWS account mismatch')
capa=aws('sts','assume-role',RoleArn=s['capaRoleARN'],RoleSessionName=s['runID'],DurationSeconds=3600)
private_write(work/'capa-session.json',json.dumps(capa))
values_path=work/'iam-values.json'
values=json.loads(values_path.read_text());values['ASSET_BUCKET']=s['assetBucket']
values_path.write_text(json.dumps(values,indent=2)+'\n')
iam=pathlib.Path(__file__).resolve().parents[3]/'docs'/'iam'
policy=subprocess.check_output(['python3',str(iam/'render.py'),str(iam/'harness.json'),str(values_path)],text=True)
(work/'harness-policy.json').write_text(policy)
# STS also has a compressed-policy limit: the two-document harness policy
# reached 101% on GetFederationToken despite being under 2048 characters.
# Statement IDs have no authorization semantics; omit them from the wire form.
session_policy=json.loads(policy)
for statement in session_policy['Statement']:
 statement.pop('Sid',None)
wire_policy=json.dumps(session_policy,separators=(',',':'))
if len(wire_policy)>2048:raise RuntimeError('harness session policy exceeds STS 2048-character limit')
harness=aws('sts','get-federation-token',Name='cldt-harness',DurationSeconds=3600,Policy=wire_policy)
private_write(work/'harness-session.json',json.dumps(harness))
s['sessionExpiresAt']=capa['Credentials']['Expiration'];s['harnessSessionExpiresAt']=harness['Credentials']['Expiration']
path.write_text(json.dumps(s,indent=2)+'\n')
print('Renewed derived sessions; CAPA expires',s['sessionExpiresAt'])
