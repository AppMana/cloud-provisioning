"""Check identity replacement under the same claim name from native snapshots."""
import argparse
import json
import pathlib

FRESH = ('claimUID','machineUID','infrastructureUID','nodeUID','instanceID',
         'providerID','bootstrapSecretUID','adoptionSecretUID','adoptionPodUID','publicKeySHA256')


def evaluate(before, after, removal):
    checks = {'sameClaimName':bool(before.get('claim')) and before.get('claim')==after.get('claim'),
              'sameTemplate':bool(before.get('templateUID')) and before.get('templateUID')==after.get('templateUID'),
              'retiredInstanceTerminated':removal.get('terminated') is True and removal.get('productCleanup') is True,
              'retirementMatchesOriginal':all(removal.get(k)==before.get(k) and bool(before.get(k))
                    for k in ('claim','nodeUID','instanceID','providerID')),
              'singleNIC':all(type(s.get('eniCount')) is int and s['eniCount']==1 for s in (before,after))}
    for key in FRESH:
        checks['fresh'+key[0].upper()+key[1:]]=all(isinstance(s.get(key),str) and bool(s[key]) for s in (before,after)) and before[key]!=after[key]
    for name, state in [('original',before),('replacement',after)]:
        checks[name+'AppliedPeers']=bool(state.get('desiredPeerSHA256')) and state.get('desiredPeerSHA256')==state.get('appliedPeerSHA256')
    return dict(passed=all(checks.values()),checks=checks,
        scope='Deleted/recreated claim identity, retired-instance receipt and applied-peer snapshots; survivor traffic requires a separately bracketed observation window')


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for key in ['before','after','removal','output']:p.add_argument('--'+key,type=pathlib.Path,required=True)
    a=p.parse_args();r=evaluate(*[json.loads(path.read_text()) for path in [a.before,a.after,a.removal]])
    with a.output.open('x') as stream:json.dump(r,stream,indent=2)
    print(json.dumps(r));return 0 if r['passed'] else 1

if __name__=='__main__':raise SystemExit(main())
