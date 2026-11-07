package apphost

import "context"

// InteractionSubjectProvider binds a previously observed subject to its
// application-specific content version. Model arguments cannot supply it.
// Each adapter keeps its own platform IDs and evidence storage private.
type InteractionSubjectProvider interface {
	ResolveInteractionSubject(context.Context, InteractionDraft) (string, error)
}
