import { fileURLToPath, URL } from 'node:url'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
    plugins: [react()],
    publicDir: 'public',
    base: '/',
    build: {
        outDir: 'dist'
    },
    resolve: {
        tsconfigPaths: true,
        alias: [
            {
                find: /^\.\/dompurify\/dompurify\.js$/,
                replacement: fileURLToPath(new URL('./src/shared/lib/monaco-dompurify.ts', import.meta.url))
            },
            { find: '@entitites', replacement: fileURLToPath(new URL('./src/entitites', import.meta.url)) },
            { find: '@features', replacement: fileURLToPath(new URL('./src/features', import.meta.url)) },
            { find: '@pages', replacement: fileURLToPath(new URL('./src/pages', import.meta.url)) },
            { find: '@widgets', replacement: fileURLToPath(new URL('./src/widgets', import.meta.url)) },
            { find: '@', replacement: fileURLToPath(new URL('./src', import.meta.url)) },
            { find: '@public', replacement: fileURLToPath(new URL('./public', import.meta.url)) },
            { find: '@shared', replacement: fileURLToPath(new URL('./src/shared', import.meta.url)) }
        ]
    }
})
