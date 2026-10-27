"""What the product left in a run's AWS account after its workers are gone.

Cleanup deletes the run's network wholesale, which would also delete state the
product should have removed itself. This lists that state first, so a leak is
reported rather than hidden: return routes and tagged ingress rules from the
gateway adapter, interfaces left behind in the VPC, and CAPA bootstrap secrets
of the run's cluster.
"""

# Routes the harness itself creates in the run's route table.
HARNESS_ROUTES = {('DestinationCidrBlock', '0.0.0.0/0'), ('DestinationIpv6CidrBlock', '::/0')}
INGRESS_OWNER = 'cloud-provisioning.appmana.com/ingress-owner'


def product_leaks(aws, s):
    leaks = []
    if s.get('routeTableID'):
        tables = aws('ec2', 'describe-route-tables', '--route-table-ids', s['routeTableID'], absent_ok=True).get('RouteTables', [])
        for table in tables:
            for route in table.get('Routes', []):
                if route.get('GatewayId') == 'local':
                    continue
                key = next(((k, route[k]) for k in ('DestinationCidrBlock', 'DestinationIpv6CidrBlock') if k in route), None)
                if key in HARNESS_ROUTES:
                    continue
                leaks.append({'kind': 'route', 'routeTable': s['routeTableID'], 'destination': key[1] if key else None,
                              'target': route.get('NetworkInterfaceId') or route.get('InstanceId')})
    if s.get('securityGroupID'):
        rules = aws('ec2', 'describe-security-group-rules', '--filters', f'Name=group-id,Values={s["securityGroupID"]}', absent_ok=True)
        for rule in rules.get('SecurityGroupRules', []):
            if any(tag['Key'] == INGRESS_OWNER for tag in rule.get('Tags', [])):
                leaks.append({'kind': 'ingress-rule', 'securityGroup': s['securityGroupID'], 'rule': rule['SecurityGroupRuleId']})
    if s.get('vpcID'):
        interfaces = aws('ec2', 'describe-network-interfaces', '--filters', f'Name=vpc-id,Values={s["vpcID"]}', absent_ok=True)
        for eni in interfaces.get('NetworkInterfaces', []):
            leaks.append({'kind': 'network-interface', 'id': eni['NetworkInterfaceId'], 'status': eni.get('Status')})
    secrets = aws('secretsmanager', 'list-secrets', '--filters',
                  f'Key=tag-key,Values=sigs.k8s.io/cluster-api-provider-aws/cluster/{s["runID"]}', absent_ok=True)
    for secret in secrets.get('SecretList', []):
        leaks.append({'kind': 'bootstrap-secret', 'arn': secret['ARN']})
    return leaks
