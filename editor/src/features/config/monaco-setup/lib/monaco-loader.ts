import JsonWorker from 'monaco-editor/languages/features/json/json.worker?worker'
import 'monaco-editor/editor/contrib/codelens/browser/codeLensCache'
import 'monaco-editor/editor/common/services/treeViewsDndService'
import 'monaco-editor/features/documentSymbols/register'
import EditorWorker from 'monaco-editor/editor/editor.worker?worker'
import * as monaco from 'monaco-editor/editor'
import { loader } from '@monaco-editor/react'
import 'monaco-editor/features/bracketMatching/register'
import 'monaco-editor/features/clipboard/register'
import 'monaco-editor/features/codeEditor/register'
import 'monaco-editor/features/comment/register'
import 'monaco-editor/features/contextmenu/register'
import 'monaco-editor/features/find/register'
import 'monaco-editor/features/folding/register'
import 'monaco-editor/features/format/register'
import 'monaco-editor/features/gotoError/register'
import 'monaco-editor/features/hover/register'
import 'monaco-editor/features/indentation/register'
import 'monaco-editor/features/linesOperations/register'
import 'monaco-editor/features/links/register'
import 'monaco-editor/features/multicursor/register'
import 'monaco-editor/features/suggest/register'
import 'monaco-editor/editor/contrib/suggest/browser/suggestController'
import 'monaco-editor/features/wordOperations/register'

self.MonacoEnvironment = {
    getWorker(_, label) {
        return label === 'json' ? new JsonWorker() : new EditorWorker()
    }
}

loader.config({ monaco })
