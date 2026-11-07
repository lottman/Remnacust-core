import { jsonDefaults } from 'monaco-editor/languages/features/json/register'
import { consola } from 'consola/browser'
import axios from 'axios'

export const MonacoSetupFeature = {
    setup: async () => {
        try {
            const response = await axios.get('xray.schema.json')
            const schema = await response.data

            jsonDefaults.setDiagnosticsOptions({
                allowComments: false,
                enableSchemaRequest: true,
                schemaRequest: 'warning',
                schemas: [
                    {
                        fileMatch: ['*'],
                        schema,
                        uri: 'https://xray-config-schema.json'
                    }
                ],
                validate: true
            })
        } catch (error) {
            consola.error('Failed to load JSON schema:', error)
        }
    }
}
