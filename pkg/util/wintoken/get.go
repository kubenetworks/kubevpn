package wintoken

import (
	"golang.org/x/sys/windows"
	"syscall"
	"unsafe"
)

func GetIntegrityLevelToken(wns string) (windows.Token, error) {
	var procToken syscall.Token
	var token windows.Token

	proc, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(proc)

	err = syscall.OpenProcessToken(proc,
		syscall.TOKEN_DUPLICATE|
			syscall.TOKEN_ADJUST_DEFAULT|
			syscall.TOKEN_QUERY|
			syscall.TOKEN_ASSIGN_PRIMARY,
		&procToken)
	if err != nil {
		return 0, err
	}
	defer procToken.Close()

	sid, err := syscall.StringToSid(wns)
	if err != nil {
		return 0, err
	}

	tml := &windows.Tokenmandatorylabel{}
	tml.Label.Attributes = windows.SE_GROUP_INTEGRITY
	tml.Label.Sid = (*windows.SID)(sid)

	err = windows.DuplicateTokenEx(windows.Token(procToken), 0, nil, windows.SecurityImpersonation,
		windows.TokenPrimary, &token)
	if err != nil {
		return 0, err
	}

	err = windows.SetTokenInformation(token,
		syscall.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(tml)),
		tml.Size())
	if err != nil {
		token.Close()
		return 0, err
	}
	return token, nil
}
