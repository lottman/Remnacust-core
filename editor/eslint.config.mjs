import { createTypeScriptImportResolver } from 'eslint-import-resolver-typescript'
import { defineConfig, globalIgnores } from 'eslint/config'
import perfectionist from 'eslint-plugin-perfectionist'
import refreshPlugin from 'eslint-plugin-react-refresh'
import reactHooks from 'eslint-plugin-react-hooks'
import * as importX from 'eslint-plugin-import-x'
import prettier from 'eslint-config-prettier'
import * as tseslint from 'typescript-eslint'
import globals from 'globals'
import js from '@eslint/js'

export default defineConfig([
    globalIgnores(['dist/**', 'node_modules/**', 'go/**', 'public/**']),
    js.configs.recommended,
    tseslint.configs.recommended,
    importX.flatConfigs.recommended,
    importX.flatConfigs.typescript,
    prettier,
    {
        files: ['src/**/*.{ts,tsx}'],
        languageOptions: { globals: globals.browser },
        plugins: { 'react-hooks': reactHooks, 'react-refresh': refreshPlugin },
        rules: {
            ...reactHooks.configs.flat.recommended.rules,
            'react-refresh/only-export-components': ['warn', { allowConstantExport: true }]
        }
    },
    {
        files: ['*.{js,cjs,mjs,ts}', 'tests/**/*.{js,mjs}'],
        languageOptions: { globals: globals.node }
    },
    {
        plugins: { perfectionist },
        settings: {
            'import-x/core-modules': [
                'monaco-editor/editor/editor.worker?worker',
                'monaco-editor/languages/features/json/json.worker?worker'
            ],
            'import-x/resolver-next': [createTypeScriptImportResolver({ project: './tsconfig.json' })]
        },
        rules: {
            'perfectionist/sort-imports': [
                'error',
                {
                    type: 'line-length',
                    order: 'desc',
                    internalPattern: ['^@(?:/|features/|pages/|widgets/|shared/|entitites/)']
                }
            ],
            'import-x/no-unresolved': ['error', { ignore: ['\\?worker$'] }],
            'import-x/no-extraneous-dependencies': 'off',
            'import-x/prefer-default-export': 'off',
            'import-x/extensions': 'off',
            '@typescript-eslint/no-empty-object-type': ['error', { allowObjectTypes: 'always' }]
        }
    }
])
