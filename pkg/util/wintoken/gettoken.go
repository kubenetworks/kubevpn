//go:build windows
// +build windows

package wintoken

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	WTS_CURRENT_SERVER_HANDLE windows.Handle = 0
)

// OpenProcessToken opens a process token using PID, pass 0 as PID for self token
func OpenProcessToken(pid int, tokenType tokenType) (*Token, error) {
	var (
		t               windows.Token
		duplicatedToken windows.Token
		procHandle      windows.Handle
		err             error
	)

	if pid == 0 {
		procHandle = windows.CurrentProcess()
	} else {
		procHandle, err = windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	}
	if err != nil {
		return nil, err
	}

	if err = windows.OpenProcessToken(procHandle, windows.TOKEN_ALL_ACCESS, &t); err != nil {
		return nil, err
	}

	defer windows.CloseHandle(windows.Handle(t))

	switch tokenType {
	case TokenPrimary:
		if err := windows.DuplicateTokenEx(t, windows.MAXIMUM_ALLOWED, nil, windows.SecurityDelegation, windows.TokenPrimary, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}
	case TokenImpersonation:
		if err := windows.DuplicateTokenEx(t, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}

	case TokenLinked:
		if err := windows.DuplicateTokenEx(t, windows.MAXIMUM_ALLOWED, nil, windows.SecurityDelegation, windows.TokenPrimary, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}
		dt, err := duplicatedToken.GetLinkedToken()
		windows.CloseHandle(windows.Handle(duplicatedToken))
		if err != nil {
			return nil, fmt.Errorf("error while getting LinkedToken: %w", err)
		}
		duplicatedToken = dt
	}

	return &Token{token: duplicatedToken, typ: tokenType}, nil
}

// GetInteractiveToken gets the interactive token associated with current logged in user
// It uses windows API WTSEnumerateSessions, WTSQueryUserToken and DuplicateTokenEx to return a valid wintoken
func GetInteractiveToken(tokenType tokenType) (*Token, error) {

	switch tokenType {
	case TokenPrimary, TokenImpersonation, TokenLinked:
	default:
		return nil, ErrOnlyPrimaryImpersonationTokenAllowed
	}

	var (
		sessionPointer   uintptr
		sessionCount     uint32
		interactiveToken windows.Token
		duplicatedToken  windows.Token
		sessionID        uint32
	)

	err := windows.WTSEnumerateSessions(WTS_CURRENT_SERVER_HANDLE, 0, 1, (**windows.WTS_SESSION_INFO)(unsafe.Pointer(&sessionPointer)), &sessionCount)
	if err != nil {
		return nil, fmt.Errorf("error while enumerating sessions: %v", err)
	}
	defer windows.WTSFreeMemory(sessionPointer)

	sessions := make([]*windows.WTS_SESSION_INFO, sessionCount)
	size := unsafe.Sizeof(windows.WTS_SESSION_INFO{})

	for i := range sessions {
		sessions[i] = (*windows.WTS_SESSION_INFO)(unsafe.Pointer(sessionPointer + (size * uintptr(i))))
	}

	for i := range sessions {
		if sessions[i].State == windows.WTSActive {
			sessionID = sessions[i].SessionID
			break
		}
	}
	if sessionID == 0 {
		return nil, ErrNoActiveSession
	}

	if err := windows.WTSQueryUserToken(sessionID, &interactiveToken); err != nil {
		return nil, fmt.Errorf("error while WTSQueryUserToken: %w", err)
	}

	defer windows.CloseHandle(windows.Handle(interactiveToken))

	switch tokenType {
	case TokenPrimary:
		if err := windows.DuplicateTokenEx(interactiveToken, windows.MAXIMUM_ALLOWED, nil, windows.SecurityDelegation, windows.TokenPrimary, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}
	case TokenImpersonation:
		if err := windows.DuplicateTokenEx(interactiveToken, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}
	case TokenLinked:
		if err := windows.DuplicateTokenEx(interactiveToken, windows.MAXIMUM_ALLOWED, nil, windows.SecurityDelegation, windows.TokenPrimary, &duplicatedToken); err != nil {
			return nil, fmt.Errorf("error while DuplicateTokenEx: %w", err)
		}
		dt, err := duplicatedToken.GetLinkedToken()
		windows.CloseHandle(windows.Handle(duplicatedToken))
		if err != nil {
			return nil, fmt.Errorf("error while getting LinkedToken: %w", err)
		}
		duplicatedToken = dt
	}

	if windows.Handle(duplicatedToken) == windows.InvalidHandle {
		return nil, ErrInvalidDuplicatedToken
	}

	return &Token{typ: tokenType, token: duplicatedToken}, nil
}
