package session

import "context"

type admissionKey struct{}

// Admission keeps authorization and link registration in one commit boundary.
// The returned lease must be released before any transport I/O.
type Admission func(context.Context) (release func(), err error)

func ContextWithAdmission(ctx context.Context, admission Admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, admission)
}

func AdmissionFromContext(ctx context.Context) Admission {
	admission, _ := ctx.Value(admissionKey{}).(Admission)
	return admission
}
