package wintoken

import "fmt"

var (
	ErrNoActiveSession                      error = fmt.Errorf("no active session found")
	ErrInvalidDuplicatedToken               error = fmt.Errorf("invalid duplicated token")
	ErrOnlyPrimaryImpersonationTokenAllowed error = fmt.Errorf("only primary or impersonation token types allowed")
	ErrNoPrivilegesSpecified                error = fmt.Errorf("no privileges specified")
	ErrTokenClosed                          error = fmt.Errorf("token has been closed")
)
