import argparse,pathlib,subprocess,json,os,time,datetime,hashlib,uuid
os.umask(0o077)
parser=argparse.ArgumentParser(description='Native receive-overlap experiment on the retained three-VM site topology')
parser.add_argument('--outer',required=True,help='Outer container holding the single-NIC QEMU VM rig')
parser.add_argument('--lab',default='cldt')
parser.add_argument('--work-dir',required=True,type=pathlib.Path,help='New evidence directory; existing directories are refused')
parser.add_argument('--binary',required=True,type=pathlib.Path,help='Statically compiled Linux helper')
parser.add_argument('--egress',action='store_true',help='Also exercise unbound sockets across route switches and rollback')
args=parser.parse_args()
work=args.work_dir;work.mkdir(mode=0o700)
binary=args.binary.resolve();runid=uuid.uuid4().hex[:8];exe='/tmp/cldt-hov-'+runid;ns='cldt-hov-'+runid
(work/'run.json').write_text(json.dumps(dict(namespace=ns,executable=exe,outer=args.outer,lab=args.lab,egress=args.egress,startedAt=datetime.datetime.now(datetime.timezone.utc).isoformat()),indent=2))
keys=json.loads(subprocess.check_output([str(binary),'-mode','keys']));(work/'keys.private.json').write_text(json.dumps(keys))
now=lambda:datetime.datetime.now(datetime.timezone.utc).isoformat()
def prefix(node,has_input=False):return ['docker','exec','-i',args.outer,'docker','exec','-i','clab-'+args.lab+'-'+node,'/cldt-guest','exec','180s','1' if has_input else '0']
def call(node,args,data=None,check=True):
 r=subprocess.run(prefix(node,data is not None)+args,input=data,capture_output=True)
 if check and r.returncode:raise RuntimeError(node+': '+r.stderr.decode()[:800]+r.stdout.decode()[:800])
 return r
processes=[];events=[];created=[]
def event(name,**kwargs):events.append(dict(at=now(),event=name,**kwargs));(work/'events.json').write_text(json.dumps(events,indent=2))
def spawn(label,node,args):
 out=(work/(label+'.jsonl')).open('xb');err=(work/(label+'.stderr')).open('xb');p=subprocess.Popen(prefix(node)+['ip','netns','exec',ns,exe]+args,stdout=out,stderr=err);processes.append((label,p,out,err));event('spawn',label=label,node=node,pid=p.pid);return p
def wait(p):
 code=p.wait(timeout=170)
 if code:raise RuntimeError('native process failed: '+str(code))
def ready(node,path):
 deadline=time.monotonic()+20
 while time.monotonic()<deadline:
  if call(node,['test','-f',path],check=False).returncode==0:return
  time.sleep(.2)
 raise RuntimeError('readiness timeout '+node+' '+path)
def setup(node,device,local,peer,endpoint,create):
 addresses=['10.254.99.13/32','fd99:99::13/128'] if node=='cp2' else ['10.254.99.11/32','fd99:99::11/128','10.254.99.99/32','fd99:99::99/128']
 allowed=['10.254.99.11/32','fd99:99::11/128'] if node=='cp2' else ['10.254.99.13/32','fd99:99::13/128']
 port=54871 if device=='cldt-hov-a' else 54872
 plan=dict(Namespace=ns,Device=device,Key=keys[local]['private'],PeerKey=keys[peer]['public'],Endpoint=endpoint,Port=port,Create=create,Addresses=addresses,Allowed=allowed)
 if create:created.append(node)
 call(node,[exe,'-mode','setup'],json.dumps(plan).encode());event('configured',node=node,device=device,publicKey=keys[local]['public'],allowed=allowed)
def server(gen,family,node='cp2'):
 address=('10.254.99.13:9001' if family==4 else '[fd99:99::13]:9001') if node=='cp2' else ('10.254.99.11:9002' if family==4 else '[fd99:99::11]:9002')
 label=('server-' if node=='cp2' else 'server-egress-')+gen+'-'+str(family)
 receipt=exe+'-'+label+'.ready';p=spawn(label,node,['-mode','serve','-device','cldt-hov-'+gen,'-address',address,'-ready',receipt,'-duration','150s']);ready(node,receipt);return node,receipt
receipts=[]
def probe(label,node,gen,family,count=40,negative=False,receipt=''):
 source=('10.254.99.99' if negative else '10.254.99.11') if family==4 else ('fd99:99::99' if negative else 'fd99:99::11')
 dest='10.254.99.13:9001' if family==4 else '[fd99:99::13]:9001'
 args=['-mode','probe','-device','cldt-hov-'+gen,'-source',source,'-destination',dest,'-count',str(count),'-interval','20ms']
 if negative:args+=['-negative']
 if receipt:args+=['-ready',receipt]
 return spawn(label,node,args)
def unbound(label,family,count=400):
 receipt=exe+'-'+label+'.ready';source='10.254.99.13' if family==4 else 'fd99:99::13';dest='10.254.99.11:9002' if family==4 else '[fd99:99::11]:9002'
 proc=spawn(label,'cp2',['-mode','probe','-source',source,'-destination',dest,'-count',str(count),'-ready',receipt]);ready('cp2',receipt);return proc

def route_observation(label,want):
 for family,dest,source in [(4,'10.254.99.11','10.254.99.13'),(6,'fd99:99::11','fd99:99::13')]:
  r=call('cp2',['ip','-n',ns,'-'+str(family),'-j','route','get',dest,'from',source]);(work/(label+'-'+str(family)+'.json')).write_bytes(r.stdout);v=json.loads(r.stdout);assert len(v)==1 and v[0]['dev']=='cldt-hov-'+want

def switch(label,gen):
 event(label+'-start')
 for family,dest in [(4,'10.254.99.11/32'),(6,'fd99:99::11/128')]:call('cp2',['ip','-n',ns,'-'+str(family),'route','replace',dest,'dev','cldt-hov-'+gen,'metric','50'])
 route_observation(label,gen);event(label+'-finished')

error=None
try:
 for node in ['cp2','w1','w2']:
  info=call(node,['python3','-c','import pathlib,json,platform; print(json.dumps(dict(kernel=platform.release(),physicalNICs=[p.name for p in pathlib.Path("/sys/class/net").iterdir() if (p/"device").exists()])))']).stdout
  v=json.loads(info);assert len(v['physicalNICs'])==1;(work/(node+'-hardware.json')).write_bytes(info)
  call(node,['sh','-ec','umask 077; test ! -e '+exe+'; cat > '+exe+'; chmod 700 '+exe],binary.read_bytes())
 setup('cp2','cldt-hov-a','rxA','old','10.10.0.11:54871',True)
 setup('w1','cldt-hov-a','old','rxA','10.10.0.13:54871',True)
 receipts += [server('a',4),server('a',6)]
 # Explicit warmup is separate from the uninterrupted streams.
 for fam in [4,6]:wait(probe('warmup-a-'+str(fam),'w1','a',fam))
 old=[]
 for fam in [4,6]:
  receipt=exe+'-old-stream-'+str(fam)+'.ready';old.append(probe('old-during-prepare-'+str(fam),'w1','a',fam,400,receipt=receipt));ready('w1',receipt)
 event('prepare-b-start')
 setup('cp2','cldt-hov-b','rxB','new','10.10.0.12:54872',False)
 setup('w2','cldt-hov-b','new','rxB','10.10.0.13:54872',True)
 receipts += [server('b',4),server('b',6)]
 for fam in [4,6]:wait(probe('warmup-b-'+str(fam),'w2','b',fam))
 event('prepare-b-finished')
 for p in old:wait(p)
 # Both authorized paths must work around negative-source probes.
 for gen,node in [('a','w1'),('b','w2')]:
  for fam in [4,6]:
   wait(probe('reject-spoof-'+gen+'-'+str(fam),node,gen,fam,2,negative=True))
   wait(probe('after-spoof-'+gen+'-'+str(fam),node,gen,fam,20))
 if args.egress:
  for gen,node in [('a','w1'),('b','w2')]:receipts += [server(gen,4,node),server(gen,6,node)]
  route_observation('egress-baseline','a')
  for label,gen in [('switch-b','b'),('rollback-a','a'),('reswitch-b','b')]:
   streams=[unbound('unbound-'+label+'-'+str(f),f) for f in [4,6]]
   switch(label,gen)
   for process in streams:wait(process)
 new=[]
 for fam in [4,6]:
  receipt=exe+'-new-stream-'+str(fam)+'.ready';new.append(probe('new-during-retire-'+str(fam),'w2','b',fam,400,receipt=receipt));ready('w2',receipt)
 if args.egress:new += [unbound('unbound-retire-a-'+str(f),f) for f in [4,6]]
 event('retire-a-start');call('cp2',['ip','-n',ns,'link','delete','cldt-hov-a']);event('retire-a-finished')
 # The old authorized source must now time out, not receive an echo.
 for fam in [4,6]:
  source='10.254.99.11' if fam==4 else 'fd99:99::11';dest='10.254.99.13:9001' if fam==4 else '[fd99:99::13]:9001'
  wait(spawn('retired-a-'+str(fam),'w1',['-mode','probe','-device','cldt-hov-a','-source',source,'-destination',dest,'-count','2','-negative']))
 for p in new:wait(p)
 if args.egress:route_observation('egress-after-retirement','b')
 event('qualification-finished')
except Exception as e:error=str(e);event('failure',error=error)
finally:
 for node,receipt in receipts:call(node,['touch',receipt+'.stop'],check=False)
 for label,p,out,err in processes:
  try:code=p.wait(timeout=170)
  except subprocess.TimeoutExpired:code=None
  event('terminal',label=label,exitCode=code);out.close();err.close()
  if code != 0 and error is None: error='native process did not complete successfully: '+label
 for node in reversed(created):
  r=call(node,['ip','netns','delete',ns],check=False);event('cleanup',node=node,exitCode=r.returncode)
  if r.returncode != 0 and error is None: error='namespace cleanup failed on '+node
 result=dict(ok=error is None,error=error,finishedAt=now(),binarySHA256=hashlib.sha256(binary.read_bytes()).hexdigest());(work/'result.json').write_text(json.dumps(result,indent=2));print(json.dumps(result),flush=True)
 if error:raise SystemExit(1)
