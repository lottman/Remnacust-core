const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');
const { Worker, isMainThread, parentPort, workerData } = require('node:worker_threads');

if (!isMainThread) {
    globalThis.crypto ??= require('node:crypto').webcrypto;
    require(workerData.runtime);
    const go = new Go();
    globalThis.onWasmInitialized = () => setTimeout(() => {
        try {
            const results = workerData.configs.map(config => XrayParseConfig(JSON.stringify(config)));
            parentPort.postMessage({ version: XrayGetVersion(), results });
        } catch (error) { parentPort.postMessage({ error: error.message }); }
    }, 0);
    WebAssembly.instantiate(fs.readFileSync(workerData.wasm), go.importObject).then(result => go.run(result.instance))
        .catch(error => parentPort.postMessage({ error: error.message }));
} else {
    const assets = process.env.REMNACUST_DEFAULT_ASSETS || path.resolve(__dirname, '../../Remnacust-panel/panel/frontend/public/assets');
    const runtime = process.env.REMNACUST_WASM_RUNTIME || path.join(assets, 'wasm_exec.js');
    const schema = JSON.parse(fs.readFileSync(path.join(assets, 'xray.default.schema.json'), 'utf8'));
    const root = schema.definitions.XrayDefaultConfig;
    test('default assets retain official builders and reviewed security dependencies with stock configuration definitions', () => {
        const provenance = JSON.parse(fs.readFileSync(path.join(assets, 'xray.default.provenance.json'), 'utf8'));
        assert.equal(provenance.flavor, 'default');
        assert.equal(provenance.upstreamCommit, '676eaf66f8f05d4d558ee7a74b4736623d3feeb9');
        assert.equal(provenance.configurationSourceSha256, '01666deb53beae1df9df9fccbca8e807dc7405996cc8a368c1be6713c26e7cba');
        const security = path.resolve(__dirname, '../upstream-security');
        const manifest = fs.readFileSync(path.join(security, 'manifest.json'));
        const { manifestSha256, ...recordedManifest } = provenance.dependencySecurityUpdates;
        assert.equal(manifestSha256, createHash('sha256').update(manifest).digest('hex'));
        assert.deepEqual(recordedManifest, JSON.parse(manifest));
        for (const [name, field] of [['go.mod', 'goModSha256'], ['go.sum', 'goSumSha256']])
            assert.equal(createHash('sha256').update(fs.readFileSync(path.join(security, name))).digest('hex'), recordedManifest[field]);
        for (const [filename, sha] of Object.entries(provenance.files))
            assert.equal(createHash('sha256').update(fs.readFileSync(path.join(assets, filename))).digest('hex'), sha, filename);
        for (const match of JSON.stringify(schema).matchAll(/"\$ref":"#\/definitions\/([^"]+)"/g))
            assert(schema.definitions[match[1]], match[1]);
        assert(schema.definitions.XrayDefaultStreamConfig.properties.network.enum.includes('xhttp'));
        assert(!schema.definitions.XrayDefaultStreamConfig.properties.network.enum.includes('xera-http'));
        assert.equal(schema.definitions.XrayDefaultStreamConfig.properties.xeraHttpSettings, undefined);
        assert.deepEqual(schema.definitions.XrayDefaultStreamConfig.not, { required: ['xeraHttpSettings'] });
        assert(!schema.definitions.XrayDefaultInboundDetourConfig.properties.protocol.enum.includes('olcrtc'));
        assert(!schema.definitions.XrayDefaultXeraHTTPConfig);
        assert(root.properties.inbounds);
    });
    test('stock WASM reports the upstream version, accepts standard transports/geodata and rejects custom protocols', { timeout: 30_000 }, async t => {
        const base = { inbounds: [{ tag: 'socks', protocol: 'socks', listen: '127.0.0.1', port: 1080, settings: { auth: 'noauth', udp: true } }],
            outbounds: [{ tag: 'direct', protocol: 'freedom' }] };
        const xhttp = { ...base, inbounds: [{ tag: 'vless', protocol: 'vless', port: 1080, settings: { clients: [], decryption: 'none' },
            streamSettings: { network: 'xhttp', xhttpSettings: { path: '/test' } } }] };
        const geo = { ...base, routing: { rules: [{ type: 'field', ip: ['geoip:private'], outboundTag: 'direct' }] } };
        const xera = structuredClone(xhttp); xera.inbounds[0].streamSettings = { network: 'xera-http', xeraHttpSettings: { path: '/test' } };
        const olc = { ...base, outbounds: [{ protocol: 'olcrtc', settings: {} }] };
        const worker = new Worker(__filename, { workerData: { wasm: path.join(assets, 'default.wasm'), runtime, configs: [base, xhttp, geo, xera, olc] } });
        t.after(() => worker.terminate());
        const result = await new Promise((resolve, reject) => { worker.once('message', resolve); worker.once('error', reject); worker.once('exit', code => { if (code) reject(Error('WASM worker exited ' + code)); }); });
        assert(!result.error, result.error);
        assert.equal(result.version, '26.11.9');
        assert.deepEqual(result.results.slice(0, 3), [null, null, null]);
        for (const error of result.results.slice(3)) assert.equal(typeof error, 'string');
        assert.match(result.results[3], /transport|xera/i);
        assert.match(result.results[4], /protocol|olcrtc/i);
    });
}
