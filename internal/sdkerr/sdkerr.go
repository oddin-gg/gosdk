// Package sdkerr holds sentinel errors shared between the public gosdk
// package and internal layers. It exists to break an import cycle: the
// public sentinel (gosdk.ErrAlreadyClosed) must be MATCHABLE via
// errors.Is on errors that originate deep inside internal packages
// (api.ErrClosed on post-Close catalog calls), but internal packages
// cannot import the gosdk root. Both sides reference this leaf instead.
package sdkerr

import "errors"

// ErrClosed is the closed-client sentinel. gosdk.ErrAlreadyClosed IS
// this value; internal layers wrap it so every post-Close error path
// classifies under the single public sentinel.
var ErrClosed = errors.New("gosdk: client closed")
