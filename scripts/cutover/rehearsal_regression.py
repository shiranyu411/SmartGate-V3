import argparse,json,os,subprocess,time,urllib.request,urllib.error
from pathlib import Path
from rehearsal_api import RehearsalAPI
parser=argparse.ArgumentParser(description='Isolated OpenWrt VM regression fixture; never production')
parser.add_argument('--token-file',required=True)
parser.add_argument('--phase',choices=['setup-v2','verify-v2','verify-v3','reproduce-old-geo'],required=True)
args=parser.parse_args()
client=RehearsalAPI(args.token_file)
api,remote,save=client.api,client.remote,client.save
NODE_COUNT=160

def setup_v2():
    version=api('/api/version')
    assert version['commit']=='ee07bea',version
    spec=json.loads(remote('cat /etc/smartgate/spec.json'))
    remote('tar -czf /root/smartgate-regression/v2-before-fixture.tar.gz -C / etc/smartgate etc/sing-box\n/etc/init.d/smartgate stop')
    spec['providers']=[{'id':'fixture','name':'Regression fixture','type':'manual','enabled':True,'fetch_mode':'direct','roles':['general','ai','streaming','banking','crypto']}]
    nodes=[{'type':'socks','tag':f'fixture-{i:02d}','server':'127.0.0.1','server_port':13080,'username':f'fixture-{i:02d}','password':'fixture-secret','version':'5'} for i in range(NODE_COUNT)]
    save('/etc/smartgate/lastgood/fixture.json',{'provider_id':'fixture','count':NODE_COUNT,'saved_at':'2026-10-17T06:45:00Z','outbounds':nodes})
    spec['dns']={'strategy':'ipv4_only','fakeip_enabled':False,'fakeip_range':'','final':'proxy','direct_servers':['udp://127.0.0.1:15353'],'proxy_servers':['https://1.1.1.1/dns-query'],'fallback_servers':['udp://127.0.0.1:15353']}
    spec['remote_rules']['enabled']=True
    spec['remote_rules']['sets']=[{'id':'geosite-cn','name':'Fixture CN misclassification','enabled':True,'target':'direct','type':'local','format':'source','path':'/etc/smartgate/fixture-geosite-cn.json'}]
    save('/etc/smartgate/fixture-geosite-cn.json',{'version':3,'rules':[{'domain':['www.gstatic.com','gemini.google.com','cn.example']}]})
    for task in spec['scheduler']: task['enabled']=task['id']=='failover-watchdog'
    spec['scheduler']=[t for t in spec['scheduler'] if t['id']!='all-node-delay-2m']
    spec['scheduler'].append({'id':'all-node-delay-2m','name':'全部节点测速（每 2 分钟）','type':'delay_test','enabled':True,'schedule':'@every 2m'})
    for pool in spec['business_pools']: pool['node_ids']=[]
    save('/etc/smartgate/spec.json',spec)
    remote('/etc/init.d/smartgate start')
    for i in range(30):
     try:
      views=api('/api/nodes')['nodes'];break
     except Exception:time.sleep(1)
    else:raise RuntimeError('fixture API did not start')
    assert len(views)==NODE_COUNT,len(views)
    by_tag={n['tag']:n['id'] for n in views}
    for i,pool in enumerate(spec['business_pools']):pool['node_ids']=[by_tag[f'fixture-{2*i:02d}'],by_tag[f'fixture-{2*i+1:02d}']]
    save('/etc/smartgate/spec.json',spec)
    op='fixture-v2-baseline'
    preview=api('/api/config/dryrun',{'op_id':op})
    result=api('/api/config/apply',{'dryrun_id':preview['id']})
    for i,pool in enumerate(spec['business_pools']):api('/api/proxy-groups/'+pool['id']+'/select',{'node_id':by_tag[f'fixture-{2*i:02d}']})
    round_result=api('/api/nodes/delay-all',{})
    print(json.dumps({'version':version['commit'],'epoch':result['config_epoch'],'delay_test':round_result['delay_test'],'saved_pools':[(p['id'],len(p['node_ids'])) for p in spec['business_pools']]},ensure_ascii=False))
    assert round_result['delay_test']['ok']==NODE_COUNT,round_result['delay_test']
    print('V2 fixture baseline PASS: reachable nodes; TLS verification retained')

def state_snapshot():
    state=api('/api/config/state')
    runtime=api('/api/runtime/status')
    selections=json.loads(remote('cat /etc/smartgate/selection-state.json'))
    return state,runtime,selections

def mode(**kwargs):
    save('/root/smartgate-regression/mode.json',kwargs)

def node_views():
    return api('/api/nodes')['nodes']

def name_of(node):
    if 'tag' in node:return node['tag']
    return next(a['raw_tag'] for a in node['aliases'] if a['provider_id']=='fixture')

def selected():
    # The durable map is authoritative across RuntimeDir deletion/restarts.
    applied=json.loads(remote('cat /etc/smartgate/applied-state.json'))
    selection=json.loads(remote('cat /etc/smartgate/selection-state.json'))
    if 'pool_selections' in selection:
        return {k:v.get('smart_node') or v.get('target') for k,v in selection['pool_selections'].items()}
    return selection.get('proxy_selection',applied.get('proxy_selection',{}))

def watchdog():
    for _ in range(35):
        try:return api('/api/scheduler/failover-watchdog/run',{})['result']['failover']
        except urllib.error.HTTPError as error:
            if error.code!=409:raise
            time.sleep(1)
    raise AssertionError('watchdog stayed busy')

def verify(version_line):
    version=api('/api/version')
    assert version['version'].startswith(version_line),version
    views=node_views();assert len(views)==NODE_COUNT,len(views)
    by_name={name_of(n):n['id'] for n in views}
    spec=json.loads(remote('cat /etc/smartgate/spec.json'))
    tasks=[dict(t) for t in spec['scheduler']]
    assert next(t for t in tasks if t['id']=='all-node-delay-2m')['enabled']
    assert not next(t for t in tasks if t['id']=='node-delay-test')['enabled']
    for task in spec['scheduler']:task['enabled']=False
    save('/etc/smartgate/spec.json',spec)
    try:
        mode()
        # Repeated rehearsals may start after a previous successful switch.
        # Reset only this dummy fixture pool through its normal transaction.
        if selected()['general']!=by_name['fixture-00']:
            if version_line=='3.':
                binding=next(b for b in api('/api/runtime/status')['bindings'] if b['pool']=='general')
                api('/api/proxy-groups/general/select',{
                    'target_smart_node':by_name['fixture-00'],
                    'source':{'smart_node':binding['target']['smart_node'],'generation':binding['generation']},
                    'expected_config_epoch':api('/api/config/state')['config_epoch'],
                    'op_id':'regression-reset-'+str(time.time_ns())})
            else:
                api('/api/proxy-groups/general/select',{'node_id':by_name['fixture-00']})
        before=selected()
        started=time.monotonic();bulk=api('/api/nodes/delay-all',{})['delay_test'];elapsed=time.monotonic()-started
        assert bulk['ok']==NODE_COUNT and bulk['unknown']==0 and elapsed<30,(bulk,elapsed)
        assert selected()==before
        print('PASS full-node round',json.dumps(bulk),'elapsed_s',round(elapsed,3),flush=True)
        mode(primary_fail=['fixture-00'])
        result=watchdog();assert result['applied']==0 and selected()==before,result
        health=next(n['health'] for n in node_views() if name_of(n)=='fixture-00')
        assert health['watchdog_status']=='ok' and 'cloudflare.com' in health['test_url'],health
        print('PASS alternate target preserves current nodes',flush=True)
        mode(all_fail=['*'])
        for _ in range(2):
            result=watchdog();assert result['applied']==0 and selected()==before,result
        health=next(n['health'] for n in node_views() if name_of(n)=='fixture-00')
        streak=health['consecutive_failures'];assert streak>=2,health
        bulk=api('/api/nodes/delay-all',{})['delay_test'];assert bulk['failed']==NODE_COUNT,bulk
        health=next(n['health'] for n in node_views() if name_of(n)=='fixture-00')
        assert health['watchdog_status']=='failed' and health['consecutive_failures']==streak,health
        print('PASS all-failed rounds keep choices; bulk retains watchdog evidence',flush=True)
        mode(all_fail=['*'],delay_ms=7000)
        started=time.monotonic();bulk=api('/api/nodes/delay-all',{})['delay_test'];elapsed=time.monotonic()-started
        assert bulk['unknown']>0 and bulk['tested']+bulk['unknown']==NODE_COUNT and elapsed<30,(bulk,elapsed)
        print('PASS real round deadline',json.dumps(bulk),'elapsed_s',round(elapsed,3),flush=True)
        mode()
        if version_line=='3.':
            # A real open TLS connection through the current binding must close
            # when the legacy pool switches, even if only the alternate succeeds.
            graph=json.loads(remote('cat /etc/smartgate/applied-state.json'))['execution_graph']
            pool=next(p for p in spec['business_pools'] if p['id']=='general')
            assert pool['policy']['cutover']=='strict',pool['policy']
            binding=next(b for b in api('/api/runtime/status')['bindings'] if b['pool']=='general')
            lane=(binding['generation']-1)%2
            worker=next(w for w in graph['workers'] if w['id']==binding['target']['worker'])
            route=next(r for r in worker['routes'] if r['pool']=='general' and r['lane']==lane)
            inbound=next(i for i in worker['inbounds'] if i['route']==route['id'] and i['kind']=='socks')
            command=f"/root/smartgate-regression/probe regression-http --hold --status 200 --url https://www.gstatic.com/hold --socks 127.0.0.1:{inbound['port']} --node fixture-00"
            holder=client.start_remote(command)
            try:
                ready=holder.stdout.readline().strip();assert ready.startswith('HOLD_READY'),ready
                mode(all_fail=['fixture-00'],primary_fail=['fixture-01'])
                result=watchdog()
                assert result['applied']==1 and selected()['general']==by_name['fixture-01'],(result,selected())
                output,error=holder.communicate(timeout=10)
                assert holder.returncode==0 and 'HOLD_CLOSED' in output,(output,error)
                print('PASS strict switch closes old TLS flow; alternate verifies new path',output.strip(),flush=True)
            finally:
                if holder.poll() is None:holder.kill();holder.wait()
        else:
            mode(all_fail=['fixture-00'],primary_fail=['fixture-01'])
            result=watchdog();assert result['applied']==1 and selected()['general']==by_name['fixture-01'],(result,selected())
            print('PASS verified replacement selected',flush=True)
        mode()
        print('PASS fixture regression',version['commit'],flush=True)
    finally:
        mode()
        saved=json.loads(remote('cat /etc/smartgate/spec.json'))
        saved['scheduler']=tasks
        save('/etc/smartgate/spec.json',saved)

def reproduce_old_geo():
    version=api('/api/version');assert version['commit']=='757f15c',version
    for network in ['udp','tcp']:
        answer=remote('/root/smartgate-regression/probe dns-query -network '+network+' -name www.gstatic.com -type A')
        assert '127.0.0.1' in answer,answer
    spec=json.loads(remote('cat /etc/smartgate/spec.json'))
    general=next(p for p in spec['business_pools'] if p['id']=='general')
    assert general['policy']['cutover']=='graceful',general['policy']
    print('REPRODUCED 757f15c: Google assets use polluted Direct DNS over UDP/TCP; migrated general pool drains for 30 seconds',flush=True)

if args.phase=='setup-v2':setup_v2()
elif args.phase=='verify-v2':verify('0.1.')
elif args.phase=='verify-v3':verify('3.')
else:reproduce_old_geo()
