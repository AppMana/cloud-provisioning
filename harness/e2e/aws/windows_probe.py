#!/usr/bin/env python3
"""Launch disposable Windows userdata/SSM probes, not CAPI worker instances."""
import argparse,json,os,pathlib,subprocess
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--work-dir',required=True,type=pathlib.Path)
parser.add_argument('--userdata',required=True,type=pathlib.Path,help='EC2Launch document from cmd/windowsprobe -render-userdata')
args=parser.parse_args()
os.umask(0o077)
work=args.work_dir
path=work/'resources.json';state=json.loads(path.read_text())
def aws(service,operation,**inputs):
 r=subprocess.run(['aws','--region',state['region'],service,operation,'--cli-input-json',json.dumps(inputs),'--output','json'],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(r.stderr)
 return json.loads(r.stdout) if r.stdout.strip() else {}
if state.get('cleanedUp') or aws('sts','get-caller-identity')['Account']!=state['account']:raise RuntimeError('inactive run or wrong account')
vpc=aws('ec2','describe-vpcs',VpcIds=[state['vpcID']])['Vpcs'][0]
if {t['Key']:t['Value'] for t in vpc['Tags']}.get('cloud-provisioning-test')!=state['runID']:raise RuntimeError('wrong VPC ownership')
images={}
for year in ('2022','2025'):
 found=aws('ec2','describe-images',Owners=['amazon'],Filters=[{'Name':'name','Values':[f'Windows_Server-{year}-English-Full-Base-*']},{'Name':'state','Values':['available']},{'Name':'architecture','Values':['x86_64']}])['Images']
 if not found:raise RuntimeError('no Windows '+year+' image')
 images[year]=max(found,key=lambda i:i['CreationDate'])
userdata=args.userdata.read_text()
if not userdata.startswith('<powershell>') or len(userdata.encode())>16384:
 raise RuntimeError('expected an unencoded EC2Launch document within the EC2 userdata limit')
for year,image in images.items():
 if year in state.get('windowsProbeInstances',{}):continue
 tags=[{'Key':'cloud-provisioning-test','Value':state['runID']},{'Key':'Name','Value':state['runID']+'-windows-'+year}]
 result=aws('ec2','run-instances',ImageId=image['ImageId'],InstanceType='t3.large',MinCount=1,MaxCount=1,
  SubnetId=state['subnetID'],SecurityGroupIds=[state['securityGroupID']],IamInstanceProfile={'Name':state['instanceProfileName']},
  MetadataOptions={'HttpTokens':'required'},UserData=userdata,
  TagSpecifications=[{'ResourceType':k,'Tags':tags} for k in ('instance','volume','network-interface')],
  BlockDeviceMappings=[{'DeviceName':image['RootDeviceName'],'Ebs':{'VolumeSize':50,'VolumeType':'gp3','Encrypted':True,'DeleteOnTermination':True}}])
 item=result['Instances'][0]
 state.setdefault('windowsProbeInstances',{})[year]={'instanceID':item['InstanceId'],'imageID':image['ImageId'],'imageName':image['Name']}
 path.write_text(json.dumps(state,indent=2)+'\n')
 print('Launched Windows',year,item['InstanceId'])
