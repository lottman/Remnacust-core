import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'
import { createServer } from 'vite'
import { JSDOM } from 'jsdom'
import test from 'node:test'

test('Monaco embedded sanitizer resolves to the patched runtime and rejects executable markup', async () => {
    const server = await createServer({
        server: { middlewareMode: true },
        appType: 'custom'
    })
    const dom = new JSDOM('')
    try {
        const importer = fileURLToPath(new URL('../node_modules/monaco-editor/esm/vs/base/browser/domSanitize.js', import.meta.url))
        const source = await readFile(importer, 'utf8')
        const specifier = source.match(/import\s+\w+\s+from\s+['"]([^'"]*dompurify[^'"]*)['"]/u)?.[1]
        assert.ok(specifier, 'Monaco sanitizer dependency must be reviewed when its import changes')
        const resolved = await server.pluginContainer.resolveId(specifier, importer)
        assert.equal(resolved?.id.replaceAll('\\', '/'), fileURLToPath(new URL('../src/shared/lib/monaco-dompurify.ts', import.meta.url)).replaceAll('\\', '/'))
        const module = await server.ssrLoadModule(resolved.id)
        const sanitizer = module.default(dom.window)
        assert.equal(sanitizer.version, '3.4.17')
        const clean = sanitizer.sanitize('<img src="x" onerror="alert(1)"><svg onload="alert(2)"></svg><script>alert(3)</script><a href="javascript:alert(4)">safe</a>')
        const content = dom.window.document.createElement('div')
        content.innerHTML = clean
        assert.equal(content.querySelector('script, [onerror], [onload], [href^="javascript:"]'), null)
        assert.equal(content.querySelector('a')?.textContent, 'safe')
    } finally {
        dom.window.close()
        await server.close()
    }
})
