from pathlib import Path
import copy, json, sys
CORE=Path(__file__).resolve().parents[2]
ROOT=Path(sys.argv[2]).resolve() if len(sys.argv) > 2 else CORE.parent / 'Remnacust-panel/panel/frontend'
if not (ROOT/'package.json').is_file():
    raise SystemExit('Pass the Remnacust frontend directory as the second argument')
def write(name,data):
    (ROOT/name).write_text(data,encoding='utf-8',newline='\n')
# Preserve localized documentation while adding the exact current Go config fields.
definitions = json.loads((Path(sys.argv[1])).read_text(encoding='utf-8'))
original_xera_properties = copy.deepcopy(definitions['RemnacustCoreXeraHTTPConfig']['properties'])
tls_versions = ['', '1.0', '1.1', '1.2', '1.3']
tls_policy = definitions['RemnacustCoreTLSConfig']
for field in ['minVersion', 'maxVersion']:
    tls_policy['properties'][field] = {'type': 'string', 'enum': tls_versions}
tls_policy['allOf'] = [
    {'if': {'required': ['minVersion'], 'properties': {'minVersion': {'const': version}}},
     'then': {'properties': {'maxVersion': {'enum': ['', *tls_versions[index:]]}}}}
    for index, version in enumerate(tls_versions) if version
]
def zero_or_range(minimum, maximum):
    return {'oneOf': [{'const': 0}, {'type': 'integer', 'minimum': minimum, 'maximum': maximum}]}
http2_properties = {
    'MaxConcurrentStreams': {'type': 'integer', 'minimum': 0, 'maximum': 4096},
    'MaxDecoderHeaderTableSize': {'type': 'integer', 'minimum': 0, 'maximum': 4194303},
    'MaxEncoderHeaderTableSize': {'type': 'integer', 'minimum': 0, 'maximum': 4194303},
    'MaxReadFrameSize': zero_or_range(16384, 16777215),
    'MaxReceiveBufferPerConnection': zero_or_range(65536, 4194303),
    'MaxReceiveBufferPerStream': {'type': 'integer', 'minimum': 0, 'maximum': 4194303},
    'SendPingTimeout': {'type': 'integer', 'minimum': 0, 'maximum': 86400000000000},
    'PingTimeout': {'type': 'integer', 'minimum': 0, 'maximum': 86400000000000},
    'WriteByteTimeout': {'type': 'integer', 'minimum': 0, 'maximum': 86400000000000},
    'PermitProhibitedCipherSuites': {'const': False},
    'StrictMaxConcurrentRequests': {'const': False},
    'CountError': {'type': 'null'},
}
definitions['RemnacustCoreXeraHTTPConfig']['properties']['h2Params'] = {
    'type': 'object', 'additionalProperties': False, 'properties': http2_properties,
}
ref=lambda name: {'$ref': '#/definitions/RemnacustCore'+name}

def decimal_pattern(minimum, maximum):
    def digits(low, high):
        if not low:
            return ''
        if low == '0' * len(low) and high == '9' * len(high):
            return '[0-9]{' + str(len(low)) + '}'
        if low == high:
            return low
        if low[0] == high[0]:
            return low[0] + digits(low[1:], high[1:])
        parts = [low[0] + digits(low[1:], '9' * (len(low) - 1))]
        first, last = int(low[0]) + 1, int(high[0]) - 1
        if first <= last:
            prefix = str(first) if first == last else f'[{first}-{last}]'
            parts.append(prefix + digits('0' * (len(low) - 1), '9' * (len(low) - 1)))
        parts.append(high[0] + digits('0' * (len(low) - 1), high[1:]))
        return '(?:' + '|'.join(parts) + ')'
    parts = []
    for length in range(len(str(minimum)), len(str(maximum)) + 1):
        low = max(minimum, 0 if length == 1 else 10 ** (length - 1))
        high = min(maximum, 10 ** length - 1)
        if low <= high:
            parts.append(digits(str(low), str(high)))
    return '(?:' + '|'.join(parts) + ')'

def integer_range(minimum, maximum, default_zero=False):
    positive = r'\+?0*' + decimal_pattern(max(0, minimum), maximum)
    endpoint = '(?:' + positive + ('|-0*1|-0+' if minimum == -1 else '|-0+' if minimum == 0 else '') + ')'
    patterns = [endpoint + '(?:-' + endpoint + ')?']
    numbers = [{'type': 'integer', 'minimum': minimum, 'maximum': maximum}]
    if default_zero and minimum > 0:
        zero = r'(?:[+-]?0+)'
        patterns.append(zero + '(?:-' + zero + ')?')
        numbers.append({'const': 0})
    if minimum <= 0 or default_zero:
        patterns.append('')
    return {'anyOf': [*numbers, {'type': 'string', 'pattern': '^(?:' + '|'.join(patterns) + ')$'}]}

header = {'type': 'string', 'minLength': 3, 'maxLength': 64,
          'pattern': r'^[xX]-[!#$%&\x27*+.^_`|~0-9A-Za-z-]+$',
          'not': {'pattern': r'^[xX]-(?:[fF][oO][rR][wW][aA][rR][dD][eE][dD]-(?:[fF][oO][rR]|[hH][oO][sS][tT]|[pP][rR][oO][tT][oO])|[aA][cC][cC][eE][lL]-[bB][uU][fF][fF][eE][rR][iI][nN][gG])$'}}
padding = definitions['RemnacustCoreXeraDownlinkPaddingConfig']
padding['properties'].update({
    'header': copy.deepcopy(header),
    'token': {'type': 'string', 'minLength': 16, 'maxLength': 128, 'pattern': '^[A-Za-z0-9_-]+$'},
    'bytes': integer_range(0, 1024),
    'blockBytes': integer_range(1024, 16384),
    'budgetPercent': {'type': 'integer', 'minimum': 0, 'maximum': 100},
    'burstBytes': {'type': 'integer', 'minimum': 0, 'maximum': 1048576},
})
padding['required'] = ['header', 'token']
padding['allOf'] = [{'if': {'properties': {'burstBytes': {'minimum': 1}}, 'required': ['burstBytes']},
                     'then': {'required': ['budgetPercent'], 'properties': {'budgetPercent': {'minimum': 1}}}}]

xmux = definitions['RemnacustCoreXeraMuxConfig']
for field, maximum in {'maxConnections': 256, 'maxConcurrency': 4096, 'cMaxReuseTimes': 1048576,
                       'hMaxRequestTimes': 1048576, 'hMaxReusableSecs': 604800}.items():
    xmux['properties'][field] = integer_range(0, maximum)
xmux['properties']['hKeepAlivePeriod'] = {'type': 'integer', 'minimum': -1, 'maximum': 604800}
xmux['allOf'] = [
    {'if': {'required': ['maxConnections'], 'properties': {'maxConnections': {'not': integer_range(0, 0)}}},
     'then': {'properties': {'maxConcurrency': integer_range(0, 0)}}},
]

download = copy.deepcopy(definitions['RemnacustCoreStreamConfig'])
download['properties']['address'] = {'type': 'string', 'minLength': 1}
download['properties']['port'] = {'type': 'integer', 'minimum': 1, 'maximum': 65535}
download['required'] = ['address', 'port']
download['allOf'] = [
    {'if': {'required': ['method'], 'properties': {'method': {'type': 'string'}}},
     'then': {'properties': {'method': {'enum': ['xera', 'xera-http']}}},
     'else': {'required': ['network'], 'properties': {'network': {'enum': ['xera', 'xera-http']}}}},
]
definitions['RemnacustCoreXeraDownloadSettings'] = download

xera = definitions['RemnacustCoreXeraHTTPConfig']
properties = xera['properties']
properties['xPaddingMethod'] = {'enum': ['', 'tokenish']}
properties['mode'] = {'enum': ['', 'auto', 'stream-auto', 'packet-up', 'stream-up', 'stream-one']}
properties['xPaddingPlacement'] = {'enum': ['', 'query', 'header', 'cookie', 'queryInHeader']}
for field in ['sessionIDPlacement', 'seqPlacement']:
    properties[field] = {'enum': ['', 'path', 'query', 'header', 'cookie']}
properties['uplinkDataPlacement'] = {'enum': ['', 'auto', 'body', 'header', 'cookie']}
properties['xPaddingHeader'] = {'anyOf': [{'const': ''}, copy.deepcopy(header)]}
properties['headers'] = {
    'type': 'object',
    'propertyNames': {'pattern': r'^[!#$%&\x27*+.^_`|~0-9A-Za-z-]+$',
                      'not': {'pattern': '^[hH][oO][sS][tT]$'}},
    'additionalProperties': {'type': 'string', 'pattern': r'^[^\u0000-\u0008\u000A-\u001F\u007F]*$'},
}
properties['downloadSettings'] = {'anyOf': [{'type': 'null'}, ref('XeraDownloadSettings')]}
properties['customDownlinkPadding'] = {'anyOf': [{'type': 'null'}, ref('XeraDownlinkPaddingConfig')]}
for field in ['downloadSettings', 'customDownlinkPadding']:
    original_xera_properties[field] = {'type': ['object', 'null']}
original_xera_properties['xmux'] = {'type': ['object', 'null']}
for field, (minimum, maximum, default_zero) in {
    'xPaddingBytes': (1, 65536, True), 'scMaxEachPostBytes': (1, 16777216, True),
    'uplinkChunkSize': (1, 1048576, True), 'sessionIDLength': (1, 256, True),
    'scMinPostsIntervalMs': (-1, 2147483647, False),
    'scStreamUpServerSecs': (0, 2147483647, False),
}.items():
    properties[field] = integer_range(minimum, maximum, default_zero)
for field, maximum in {'serverMaxSessions': 65536, 'serverMaxHeaderBytes': 1048576,
                       'scMaxBufferedPosts': 65536, 'serverMaxBufferedBytes': 9223372036854775807}.items():
    properties[field] = {'type': 'integer', 'minimum': 0, 'maximum': maximum}
properties['sessionIDTable'] = {'type': 'string', 'maxLength': 256, 'pattern': '^[A-Za-z0-9_.~-]*$'}
removed = ['xPaddingObfsMode', 'noGRPCHeader', 'noSSEHeader']
xera['patternProperties'] = {
    '^' + ''.join(f'[{c.lower()}{c.upper()}]' if c.isalpha() else c for c in name) + '$': False
    for name in removed
}
extra = copy.deepcopy(xera)
extra['properties'].pop('extra', None)
for field in ['host', 'path', 'mode']:
    extra['properties'][field] = copy.deepcopy(definitions['RemnacustCoreXeraHTTPConfig']['properties'][field])
extra['properties']['mode'] = {'type': 'string'}
definitions['RemnacustCoreXeraExtra'] = extra
properties['extra'] = {'anyOf': [{'type': 'null'}, ref('XeraExtra')]}
extra['allOf'] = [
    {'if': {'properties': {'customDownlinkPadding': {'type': 'object', 'properties': {'uplink': {'const': True}}, 'required': ['uplink']}}, 'required': ['customDownlinkPadding']},
     'then': {'properties': {'uplinkDataPlacement': {'enum': ['', 'body']}}}},
]

def packet_conditions(target):
    return [
        {'if': {'properties': {target: {'type': 'object', 'properties': {'uplinkDataPlacement': {'enum': ['header', 'cookie']}}, 'required': ['uplinkDataPlacement']}}, 'required': [target]},
         'then': {'required': ['mode'], 'properties': {'mode': {'const': 'packet-up'}}}},
        {'if': {'properties': {target: {'type': 'object', 'properties': {'uplinkHTTPMethod': {'pattern': '^[gG][eE][tT]$'}}, 'required': ['uplinkHTTPMethod']}}, 'required': [target]},
         'then': {'required': ['mode'], 'properties': {'mode': {'const': 'packet-up'}}}},
        {'if': {'properties': {'mode': {'const': 'stream-one'}}, 'required': ['mode']},
         'then': {'properties': {target: {'properties': {'downloadSettings': {'type': 'null'}}}}}},
    ]

def direct_conditions():
    return [
        {'if': {'properties': {'uplinkDataPlacement': {'enum': ['header', 'cookie']}}, 'required': ['uplinkDataPlacement']},
         'then': {'required': ['mode'], 'properties': {'mode': {'const': 'packet-up'}}}},
        {'if': {'properties': {'uplinkHTTPMethod': {'pattern': '^[gG][eE][tT]$'}}, 'required': ['uplinkHTTPMethod']},
         'then': {'required': ['mode'], 'properties': {'mode': {'const': 'packet-up'}}}},
        {'if': {'properties': {'mode': {'const': 'stream-one'}}, 'required': ['mode']},
         'then': {'properties': {'downloadSettings': {'type': 'null'}}}},
        *copy.deepcopy(extra['allOf']),
    ]

effective = copy.deepcopy(properties)
effective.pop('extra')
for field in list(properties):
    if field not in ['extra', 'host', 'path', 'mode']:
        properties[field] = copy.deepcopy(original_xera_properties[field])
xera['allOf'] = [
    {'if': {'required': ['extra']}, 'then': {'allOf': packet_conditions('extra')},
     'else': {'properties': effective, 'allOf': direct_conditions()}},
]
def merge_props(old, new):
    for key, value in new.items():
        metadata = {k:v for k,v in old.get(key, {}).items() if k in ('description','markdownDescription','title','examples','default')}
        old[key] = {**value, **metadata}
def conditional(protocol, config):
    return {'if': {'properties': {'protocol': {'const':protocol}}, 'required':['protocol']}, 'then': {'properties': {'settings':ref(config)}}}
def mask_conditions(mapping):
    return [{'if': {'properties': {'type': {'const':kind}}, 'required':['type']}, 'then': {'properties': {'settings':ref(config)}}} for kind,config in mapping.items()]
tcp={'filter':'Filter','header-custom':'HeaderCustomTCP','fragment':'FragmentMask','sudoku':'Sudoku','xmc':'XMC'}
udp={'filter':'Filter','header-custom':'HeaderCustomUDP','mkcp-legacy':'MkcpLegacy','noise':'NoiseMask','salamander':'Salamander','sudoku':'Sudoku','xdns':'XDNS','xicmp':'Xicmp','realm':'Realm','udphop':'UDPHop'}
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
    for old,new in [('DnsObject','DNSConfig'),('RoutingObject','RouterConfig'),('TLSObject','TLSConfig'),('InboundObject','InboundDetourConfig'),('OutboundObject','OutboundDetourConfig'),('MuxObject','MuxConfig'),('SocketObject','SocketConfig'),('SniffingObject','SniffingConfig')]:
        if old not in defs or 'RemnacustCore'+new not in definitions: continue
        for key,value in definitions['RemnacustCore'+new]['properties'].items():
            defs[old].setdefault('properties',{}).setdefault(key,value)
    # The current native core does not implement this old TLS bypass.
    defs['TLSObject']['properties'].pop('allowInsecure',None)
    merge_props(defs['TLSObject']['properties'], {field: tls_policy['properties'][field] for field in ['minVersion', 'maxVersion']})
    defs['TLSObject']['allOf'] = list({json.dumps(value, sort_keys=True): value for value in [*defs['TLSObject'].get('allOf', []), *tls_policy['allOf']]}.values())
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
