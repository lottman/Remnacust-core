from pathlib import Path
import json, sys
CORE=Path(__file__).resolve().parents[2]
ROOT=Path(sys.argv[2]).resolve() if len(sys.argv) > 2 else CORE.parent / 'Remnacust-panel/panel/frontend'
if not (ROOT/'package.json').is_file():
    raise SystemExit('Pass the Remnacust frontend directory as the second argument')
def write(name,data):
    (ROOT/name).write_text(data,encoding='utf-8',newline='\n')
# Preserve localized documentation while adding the exact current Go config fields.
definitions = json.loads((Path(sys.argv[1])).read_text(encoding='utf-8'))
ref=lambda name: {'$ref': '#/definitions/RemnacustCore'+name}
def merge_props(old, new):
    for key, value in new.items():
        metadata = {k:v for k,v in old.get(key, {}).items() if k in ('description','markdownDescription','title','examples','default')}
        old[key] = {**value, **metadata}
def conditional(protocol, config):
    return {'if': {'properties': {'protocol': {'const':protocol}}, 'required':['protocol']}, 'then': {'properties': {'settings':ref(config)}}}
def mask_conditions(mapping):
    return [{'if': {'properties': {'type': {'const':kind}}, 'required':['type']}, 'then': {'properties': {'settings':ref(config)}}} for kind,config in mapping.items()]
tcp={'header-custom':'HeaderCustomTCP','fragment':'FragmentMask','sudoku':'Sudoku','xmc':'XMC'}
udp={'header-custom':'HeaderCustomUDP','mkcp-legacy':'MkcpLegacy','noise':'NoiseMask','salamander':'Salamander','sudoku':'Sudoku','xdns':'XDNS','xicmp':'Xicmp','realm':'Realm','udphop':'UDPHop'}
for language in ['', '.cn']:
    name='public/assets/xray.schema'+language+'.json'
    schema=json.loads((ROOT/name).read_text(encoding='utf-8-sig'))
    defs=schema['definitions']; defs.update(definitions)
    defs.pop('RemnacustCoreOlcrtcConfig',None)
    stream=defs['StreamSettingsObject']
    merge_props(stream['properties'], definitions['RemnacustCoreStreamConfig']['properties'])
    stream['properties']['network']['enum']=['raw','tcp','xhttp','splithttp','xera','xera-http','kcp','mkcp','grpc','ws','websocket','httpupgrade','hysteria','masque','xdrive']
    inbound_configs={'tunnel':'DokodemoConfig','dokodemo-door':'DokodemoConfig','http':'HTTPServerConfig','shadowsocks':'ShadowsocksServerConfig','mixed':'SocksServerConfig','socks':'SocksServerConfig','vless':'VLessInboundConfig','vmess':'VMessInboundConfig','trojan':'TrojanServerConfig','wireguard':'WireGuardConfig','hysteria':'HysteriaServerConfig','masque':'MasqueServerConfig','tun':'TunConfig'}
    outbound_configs={'block':'BlackholeConfig','blackhole':'BlackholeConfig','loopback':'LoopbackConfig','direct':'FreedomConfig','freedom':'FreedomConfig','http':'HTTPClientConfig','shadowsocks':'ShadowsocksClientConfig','socks':'SocksClientConfig','vless':'VLessOutboundConfig','vmess':'VMessOutboundConfig','trojan':'TrojanClientConfig','hysteria':'HysteriaClientConfig','masque':'MasqueClientConfig','dns':'DNSOutboundConfig','wireguard':'WireGuardConfig'}
    for obj, configs in [('InboundObject',inbound_configs), ('OutboundObject',outbound_configs)]:
        target=defs[obj]
        target.setdefault('allOf', []).extend(conditional(p,c) for p,c in configs.items())
        # New protocol settings must not fail an older anyOf union before the conditional applies.
        for field in ['InboundConfigurationObject' if obj=='InboundObject' else 'OutboundConfigurationObject']:
            defs[field]['anyOf'].extend(ref(c) for c in configs.values())
        protocol=target['properties']['protocol']
        if 'enum' in protocol:
            protocol['enum']=list(dict.fromkeys([*protocol['enum'],*configs.keys()]))
    for key, mapping in [('TCPMask',tcp),('UDPMask',udp)]:
        target=defs[key]
        target.setdefault('allOf', []).extend(mask_conditions(mapping))
        # Exact mask discriminator values, with native Build() providing validation.
        target['properties']['type']['enum']=list(mapping)
    # The previous finalmask schema has several examples as alternatives.
    final=defs['FinalMaskObject']
    for part in final.get('anyOf',[final]):
        merge_props(part.setdefault('properties',{}), definitions['RemnacustCoreFinalMask']['properties'])
        part['properties']['tcp']={'type':'array','items':{'$ref':'#/definitions/TCPMask'}}
        part['properties']['udp']={'type':'array','items':{'$ref':'#/definitions/UDPMask'}}
    for part in defs['RuleObject'].get('anyOf', [defs['RuleObject']]):
        if 'outboundTag' in part.get('properties',{}) or 'balancerTag' in part.get('properties',{}):
            part['properties']['localOS']={'oneOf':[{'type':'string'},{'type':'array','items':{'type':'string'}}]}
    for target in [schema,*defs.values()]:
        if isinstance(target,dict):
            for key in ['allOf','anyOf']:
                if key in target: target[key]=list({json.dumps(v,sort_keys=True):v for v in target[key]}.values())
    root=defs[schema['$ref'].split('/')[-1]]
    for key,value in definitions['RemnacustCoreConfig']['properties'].items():
        root.setdefault('properties',{}).setdefault(key,value)
    for old,new in [('DnsObject','DNSConfig'),('TLSObject','TLSConfig'),('InboundObject','InboundDetourConfig'),('OutboundObject','OutboundDetourConfig'),('MuxObject','MuxConfig'),('SocketObject','SocketConfig'),('SniffingObject','SniffingConfig')]:
        if old not in defs or 'RemnacustCore'+new not in definitions: continue
        for key,value in definitions['RemnacustCore'+new]['properties'].items():
            defs[old].setdefault('properties',{}).setdefault(key,value)
    # The current native core does not implement this old TLS bypass.
    defs['TLSObject']['properties'].pop('allowInsecure',None)
    source=json.loads((CORE/'xray/REMNACUST-UPSTREAM.json').read_text(encoding='utf-8'))
    schema['x-remnacust-upstream']={'version':source['upstreamVersion'],'commit':source['upstreamCommit'],
        'patchCommit':source.get('upstreamPatchCommit'),'coreVersion':source['version']}
    # Fail immediately on unresolved Go or original schema references.
    def check(value):
        if isinstance(value,dict):
            if '$ref' in value and value['$ref'].startswith('#/definitions/'):
                assert value['$ref'].split('/')[-1] in defs, value['$ref']
            for child in value.values(): check(child)
        elif isinstance(value,list):
            for child in value:check(child)
    check(schema)
    write(name, json.dumps(schema,ensure_ascii=False,indent=2)+'\n')
