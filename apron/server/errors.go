package server

import (
	"errors"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// internalError is what a client is told about an error that is not an
// *Error: nothing, as its text may be about the server's internals.
func internalError() *apron.Error {
	return &apron.Error{Code: apron.CodeInternalError, Message: "Internal server error"}
}

// asError returns err as an *apron.Error, or nil and false when it is not
// one.
func asError(err error) (*apron.Error, bool) {
	if e, ok := errors.AsType[*apron.Error](err); ok && e != nil {
		return e, true
	}
	return nil, false
}

// cloneUser returns a copy of u that shares nothing with it.
func cloneUser(u apron.User) apron.User {
	u.Roles = slices.Clone(u.Roles)
	u.Ext = core.CloneExt(u.Ext)
	u.Unknown = core.CloneRaw(u.Unknown)
	return u
}
