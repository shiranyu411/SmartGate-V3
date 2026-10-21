"""Exercise the installed V3 data plane only on the local rehearsal VM."""
import argparse,json,time,urllib.error
from rehearsal_api import RehearsalAPI
parser=argparse.ArgumentParser()
parser.add_argument('--token-file',required=True)
args=parser.parse_args()
client=RehearsalAPI(args.token_file)
api,remote,save=client.api,client.remote,client.save

assert api('/api/version')['version'].startswith('3.')
def spec_read():return json.loads(remote('cat /etc/smartgate/spec.json'))
def selections():return json.loads(remote('cat /etc/smartgate/selection-state.json'))['proxy_selection']
def wait_api(timeout=35):
    until=time.monotonic()+timeout
    while time.monotonic()<until:
        try:
            result=api('/api/config/state')
            if result:return result
        except Exception:pass
        time.sleep(1)
    raise AssertionError('management API did not recover')
def healthy():
    report=api('/api/system/network/diagnostics')
    assert report['status']=='ok',[(c['id'],c['ok']) for c in report['checks']]
    return report

def dns_matrix():
    probe='/root/smartgate-regression/probe dns-query'
    for protocol in ['udp','tcp']:
        answer=remote(f'{probe} -network {protocol} -name www.gstatic.com -type A')
        assert '9.9.9.9' in answer and '127.0.0.1' not in answer,answer
        remote(f'{probe} -network {protocol} -name www.gstatic.com -type AAAA')
        for kind in ['HTTPS','SVCB']:
            remote(f'{probe} -network {protocol} -name www.gstatic.com -type {kind} -expect-rcode 5 -expect-answers 0')
    print('PASS native GEO protection plus UDP/TCP A/AAAA and HTTPS/SVCB rejection',flush=True)

original=spec_read()
tasks=[dict(t) for t in original['scheduler']]
working=json.loads(json.dumps(original))
for task in working['scheduler']:task['enabled']=False
save('/etc/smartgate/spec.json',working)
choices=selections();epoch=api('/api/config/state')['config_epoch']
try:
    healthy();dns_matrix()
    # A real LAN client uses the router DNS and transparent interception.
    remote('ip netns del sg-reg-client 2>/dev/null || true\nip link del sg-reg-host 2>/dev/null || true\nip netns add sg-reg-client\nip link add sg-reg-host type veth peer name sg-reg-peer\nip link set sg-reg-host master br-lan\nip link set sg-reg-host up\nip link set sg-reg-peer netns sg-reg-client\nip -n sg-reg-client link set lo up\nip -n sg-reg-client link set sg-reg-peer up\nip -n sg-reg-client addr add 192.168.1.199/24 dev sg-reg-peer\nip -n sg-reg-client route add default via 192.168.1.1')
    for protocol in ['udp','tcp']:
        answer=remote('ip netns exec sg-reg-client /root/smartgate-regression/probe dns-query -network '+protocol+' -server 192.168.1.1:53 -name www.gstatic.com -type A')
        assert '9.9.9.9' in answer,answer
    binding=next(b for b in api('/api/runtime/status')['bindings'] if b['pool']=='general')
    node=next(n for n in api('/api/nodes')['nodes'] if n['id']==binding['target']['smart_node'])
    fixture_name=next(a['raw_tag'] for a in node['aliases'] if a['provider_id']=='fixture')
    assert fixture_name.startswith('fixture-') and fixture_name[8:].isdigit(),fixture_name
    lan=remote('ip netns exec sg-reg-client /root/smartgate-regression/probe regression-http --dns 192.168.1.1:53 --node '+fixture_name)
    assert 'tls_verified=true' in lan,lan
    remote('ip netns exec sg-reg-client /root/smartgate-regression/probe regression-tcp --address 9.9.9.9:853 --expect-refused')
    print('PASS real LAN namespace: DNS interception, selected-node HTTPS/TLS and DoT blocking',lan.strip(),flush=True)

    def fault(label,command,timeout=20):
        time.sleep(6)
        started=time.monotonic();remote(command)
        until=started+timeout
        while time.monotonic()<until:
            try:
                report=api('/api/system/network/diagnostics')
                if report['status']=='ok':break
            except Exception:pass
            time.sleep(1)
        else:raise AssertionError(label+' did not recover')
        runtime=api('/api/runtime/status')
        assert not runtime.get('rescue_active') and selections()==choices,(label,runtime)
        assert api('/api/config/state')['config_epoch']==epoch
        print('PASS',label,'recovered_s',round(time.monotonic()-started,3),flush=True)
    fault('fw4 reload','/etc/init.d/firewall reload')
    fault('deleted owned table','nft delete table inet smartgate')
    graph=json.loads(remote('cat /etc/smartgate/applied-state.json'))['execution_graph']
    dispatch=next(d for d in graph['fabric']['dispatch'] if d['pool']=='general')
    preference=11000+dispatch['table']-1000
    fault('deleted IPv4 policy route',f"ip -4 rule del pref {preference} fwmark {hex(dispatch['mark'])}/{hex(dispatch['mark_mask'])} table {dispatch['table']}")
    fault('unloaded dnsmasq override',"for config in /var/etc/dnsmasq.conf.*; do for dir in $(sed -n 's/^conf-dir=//p' \"$config\"); do dir=${dir%%,*}; file=\"$dir/smartgate.conf\"; grep -F 'owner=smartgate:dnsmasq:smartgate-upstream' \"$file\" >/dev/null && rm \"$file\"; done; done")
    worker=next(w for w in graph['workers'] if any(r['pool']=='general' for r in w['routes']))
    fault('stopped bound worker',f"/usr/libexec/smartgate-worker-control stop {worker['id']}",35)

    dirty=spec_read()
    dirty['rules'].append({'id':'regression-dirty-domain','source':'user','kind':'domain_suffix','value':'dirty.regression.invalid','pool_id':'general','enabled':True})
    save('/etc/smartgate/spec.json',dirty)
    dirty_state=api('/api/config/state');assert dirty_state['dirty']
    for action in ['repair','restart']:
        if action=='repair':
            report=api('/api/system/network/repair',{'confirm':True});assert report['status']=='ok',report
        else:
            remote('/etc/init.d/smartgate restart');wait_api();healthy()
        after=api('/api/config/state')
        assert after['dirty'] and after['spec_hash']==dirty_state['spec_hash'] and after['config_epoch']==epoch,(action,after)
        assert selections()==choices,action
        print('PASS dirty Spec/durable choices/epoch survive',action,flush=True)
    restored=spec_read();restored['rules']=working['rules'];save('/etc/smartgate/spec.json',restored)
    assert not api('/api/config/state')['dirty']

    def apply_mode(value):
        api('/api/gateway/mode',{'mode':value})
        op='regression-mode-'+value+'-'+str(time.time_ns())
        preview=api('/api/config/dryrun',{'op_id':op})
        result=api('/api/config/apply',{'dryrun_id':preview['id']})
        return result
    for value in ['off','safe']:
        apply_mode(value)
        report=api('/api/system/network/diagnostics');assert report['status']=='disabled',report
        remote('/etc/init.d/firewall reload\n! nft list table inet smartgate >/dev/null 2>&1\n! test -e /etc/dnsmasq.d/smartgate.conf\nfor config in /var/etc/dnsmasq.conf.*; do for dir in $(sed -n \'s/^conf-dir=//p\' "$config"); do dir=${dir%%,*}; ! test -e "$dir/smartgate.conf"; done; done')
        try:api('/api/system/network/repair',{'confirm':True});raise AssertionError('disabled repair enabled proxy')
        except urllib.error.HTTPError as error:assert error.code==409,error.code
        print('PASS',value,'cleanup, inert firewall bridge and repair guard',flush=True)
        apply_mode('on');healthy();dns_matrix()
        assert selections()==choices

    api('/api/runtime/rescue/enter',{'op_id':'regression-rescue-enter'})
    assert api('/api/system/network/diagnostics')['status']=='disabled'
    try:api('/api/system/network/repair',{'confirm':True});raise AssertionError('rescue repair enabled proxy')
    except urllib.error.HTTPError as error:assert error.code in [409,423],error.code
    api('/api/runtime/rescue/clear',{'op_id':'regression-rescue-clear'})
    healthy();assert selections()==choices
    print('PASS rescue enter/repair guard/controlled clear preserve choices',flush=True)
    print('PASS network lifecycle regression',api('/api/version')['commit'],flush=True)
finally:
    remote('ip netns del sg-reg-client 2>/dev/null || true\nip link del sg-reg-host 2>/dev/null || true')
    spec=spec_read();spec['scheduler']=tasks;save('/etc/smartgate/spec.json',spec)
