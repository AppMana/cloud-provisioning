#!/usr/bin/env python3
"""Create isolated AWS test infrastructure and short-lived CAPA credentials.
Run after sourcing appmana's source-me.sh. Root credentials stay in this process;
only the assumed test role's session is written to the private work directory.
Every created resource is recorded immediately for deterministic cleanup.
"""
import argparse, json, os, pathlib, subprocess, time

p=argparse.ArgumentParser()
p.add_argument('--work-dir',required=True)
p.add_argument('--run-id',required=True)
p.add_argument('--region',default='us-west-2')
p.add_argument('--base-ami-id',help='explicit Canonical x86_64 base image to prepare for CAPA')
a=p.parse_args()
if not a.run_id.startswith('cldt-') or not all(c.isalnum() or c=='-' for c in a.run_id):
    p.error('run-id must start cldt- and contain only alphanumeric characters or hyphens')
os.umask(0o077)
work=pathlib.Path(a.work_dir).resolve();work.mkdir(parents=True,exist_ok=True)
state_path=work/'resources.json'
if state_path.exists():p.error('resources.json already exists; use its recorded resources or clean them up first')
state={'runID':a.run_id,'region':a.region,'createdAt':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())}
def save():state_path.write_text(json.dumps(state,indent=2)+'\n')
def aws(*args):
    r=subprocess.run(['aws','--region',a.region,*args,'--output','json'],capture_output=True,text=True)
    if r.returncode:raise RuntimeError(r.stderr.strip())
    return json.loads(r.stdout) if r.stdout.strip() else {}
def remember(key,value):state[key]=value;save();return value
identity=aws('sts','get-caller-identity');account=identity['Account'];state['account']=account;save()
# Resolve the exact AMI once; both the template and IAM policy use this ID.
if a.base_ami_id:
    images=aws('ec2','describe-images','--owners','099720109477','--image-ids',a.base_ami_id,
        '--filters','Name=state,Values=available','Name=architecture,Values=x86_64')['Images']
else:
    images=aws('ec2','describe-images','--owners','099720109477','--filters',
        'Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*',
        'Name=state,Values=available','Name=architecture,Values=x86_64')['Images']
if not images:raise RuntimeError('no available Canonical x86_64 base AMI found')
ami=remember('amiID',max(images,key=lambda i:i['CreationDate'])['ImageId'])
remember('baseAMIID',ami)
tags=[{'Key':'cloud-provisioning-test','Value':a.run_id},{'Key':'Name','Value':a.run_id}]
def tag_spec(kind):return json.dumps([{'ResourceType':kind,'Tags':tags}])
vpc=remember('vpcID',aws('ec2','create-vpc','--cidr-block','172.29.0.0/24','--tag-specifications',tag_spec('vpc'))['Vpc']['VpcId'])
aws('ec2','modify-vpc-attribute','--vpc-id',vpc,'--enable-dns-hostnames','{"Value":true}')
az=aws('ec2','describe-availability-zones','--filters','Name=state,Values=available')['AvailabilityZones'][0]['ZoneName']
subnet=remember('subnetID',aws('ec2','create-subnet','--vpc-id',vpc,'--cidr-block','172.29.0.0/25','--availability-zone',az,'--tag-specifications',tag_spec('subnet'))['Subnet']['SubnetId'])
aws('ec2','modify-subnet-attribute','--subnet-id',subnet,'--map-public-ip-on-launch')
igw=remember('internetGatewayID',aws('ec2','create-internet-gateway','--tag-specifications',tag_spec('internet-gateway'))['InternetGateway']['InternetGatewayId'])
aws('ec2','attach-internet-gateway','--vpc-id',vpc,'--internet-gateway-id',igw)
rt=remember('routeTableID',aws('ec2','create-route-table','--vpc-id',vpc,'--tag-specifications',tag_spec('route-table'))['RouteTable']['RouteTableId'])
remember('routeAssociationID',aws('ec2','associate-route-table','--subnet-id',subnet,'--route-table-id',rt)['AssociationId'])
aws('ec2','create-route','--route-table-id',rt,'--destination-cidr-block','0.0.0.0/0','--gateway-id',igw)
sg=remember('securityGroupID',aws('ec2','create-security-group','--vpc-id',vpc,'--group-name',a.run_id,'--description','Single-NIC cloud-provisioning WireGuard test workers','--tag-specifications',tag_spec('security-group'))['GroupId'])
aws('ec2','authorize-security-group-ingress','--group-id',sg,'--ip-permissions',json.dumps([{'IpProtocol':'udp','FromPort':51820,'ToPort':51820,'IpRanges':[{'CidrIp':'0.0.0.0/0','Description':'Authenticated WireGuard transport'}]}]))
node_role=a.run_id+'-node';capa_role=a.run_id+'-capa'
def role(name,principal):
    trust={'Version':'2012-10-17','Statement':[{'Effect':'Allow','Principal':principal,'Action':'sts:AssumeRole'}]}
    return aws('iam','create-role','--role-name',name,'--assume-role-policy-document',json.dumps(trust),'--tags',json.dumps(tags))['Role']['Arn']
node_arn=remember('nodeRoleARN',role(node_role,{'Service':'ec2.amazonaws.com'}));remember('nodeRoleName',node_role)
aws('iam','attach-role-policy','--role-name',node_role,'--policy-arn','arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore')
profile=remember('instanceProfileName',a.run_id+'-node')
aws('iam','create-instance-profile','--instance-profile-name',profile,'--tags',json.dumps(tags))
aws('iam','add-role-to-instance-profile','--instance-profile-name',profile,'--role-name',node_role)
capa_arn=remember('capaRoleARN',role(capa_role,{'AWS':identity['Arn']}));remember('capaRoleName',capa_role)
values={'AWS_ACCOUNT_ID':account,'AWS_REGION':a.region,'RUN_ID':a.run_id,
    'AMI_ID':ami,'SUBNET_ID':subnet,'SECURITY_GROUP_ID':sg,
    'NODE_ROLE_NAME':node_role,'INSTANCE_PROFILE_NAME':profile,
    'SOURCE_PRINCIPAL_ARN':identity['Arn']}
values_path=work/'iam-values.json';values_path.write_text(json.dumps(values,indent=2)+'\n')
iam_dir=pathlib.Path(__file__).resolve().parents[3]/'docs'/'iam'
for template,role_name in [('capa',capa_role),('worker-bootstrap',node_role)]:
    rendered=subprocess.run(['python3',str(iam_dir/'render.py'),str(iam_dir/(template+'.json')),str(values_path)],capture_output=True,text=True,check=True).stdout
    (work/(template+'-policy.json')).write_text(rendered)
    aws('iam','put-role-policy','--role-name',role_name,'--policy-name','cldt-test','--policy-document',rendered)
# Allow IAM trust and instance profile propagation before obtaining the session.
for attempt in range(12):
    try:
        session=aws('sts','assume-role','--role-arn',capa_arn,'--role-session-name',a.run_id,'--duration-seconds','3600')
        break
    except RuntimeError:
        if attempt==11:raise
        time.sleep(5)
(work/'capa-session.json').write_text(json.dumps(session))
remember('sessionExpiresAt',session['Credentials']['Expiration'])
print(json.dumps({k:state[k] for k in ['runID','region','vpcID','subnetID','securityGroupID','capaRoleARN','sessionExpiresAt']},indent=2))
