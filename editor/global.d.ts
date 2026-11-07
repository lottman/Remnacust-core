declare global {
    interface Window {
        Go: typeof window.Go
        onWasmInitialized?: () => void
        XrayGetVersion?: () => string

        XrayParseConfig: (config: string) => null | string
    }
}

export {}
