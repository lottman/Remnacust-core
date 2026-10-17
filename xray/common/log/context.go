package log

import "context"

type HandlerProvider interface{ LogHandler() Handler }
type handlerProviderKey struct{}
type isolatedHandlerKey struct{}

// Providers are evaluated for each record so feature construction can install
// the owning logger without replacing the process default.
func ContextWithHandlerProvider(ctx context.Context, provider HandlerProvider) context.Context {
	return context.WithValue(ctx, handlerProviderKey{}, provider)
}
func ContextWithIsolatedHandler(ctx context.Context) context.Context {
	return context.WithValue(ctx, isolatedHandlerKey{}, true)
}
func IsIsolatedHandler(ctx context.Context) bool {
	return ctx != nil && ctx.Value(isolatedHandlerKey{}) == true
}

type fixedHandler struct{ handler Handler }

func (h fixedHandler) LogHandler() Handler { return h.handler }
func ContextWithHandler(ctx context.Context, handler Handler) context.Context {
	return ContextWithHandlerProvider(ctx, fixedHandler{handler})
}
func contextHandler(ctx context.Context) Handler {
	if ctx != nil {
		if provider, ok := ctx.Value(handlerProviderKey{}).(HandlerProvider); ok {
			if handler := provider.LogHandler(); handler != nil {
				return handler
			}
		}
		// An incomplete or closed isolated profile must not borrow a neighbour.
		if IsIsolatedHandler(ctx) {
			return nil
		}
	}
	if handler := logHandler.Load(); handler != nil {
		return *handler
	}
	return nil
}
func RecordContext(ctx context.Context, message Message) {
	if handler := contextHandler(ctx); handler != nil {
		handler.Handle(message)
	}
}
func GetSeverityContext(ctx context.Context) Severity {
	if handler, ok := contextHandler(ctx).(SeverityLogger); ok {
		return handler.Severity()
	}
	return Severity_Debug
}
