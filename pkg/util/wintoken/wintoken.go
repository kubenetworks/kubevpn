//go:build windows
// +build windows

package wintoken

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modadvapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procLookupPrivilegeName        = modadvapi32.NewProc("LookupPrivilegeNameW")
	procLookupPrivilegeDisplayName = modadvapi32.NewProc("LookupPrivilegeDisplayNameW")
)

type (
	tokenType   int
	privModType int
)

const (
	PrivDisable privModType = iota
	PrivEnable
	PrivRemove
)

type Token struct {
	typ   tokenType
	token windows.Token
}

// TokenUserDetail is the structure that exposes token details
// Details contain Username, Domain, Account Type, User Profile Directory, Environment
type TokenUserDetail struct {
	Username       string
	Domain         string
	AccountType    uint32
	UserProfileDir string
	Environ        []string
}

func (t TokenUserDetail) String() string {
	return fmt.Sprintf("Username: %s, Domain: %s, Account Type: %d, UserProfileDir: %s", t.Username, t.Domain, t.AccountType, t.UserProfileDir)
}

// Privilege is the structure which exposes privilege details
// Details contain Name, Description, Enabled, EnabledByDefault, Removed, UsedForAccess
type Privilege struct {
	Name             string
	Description      string
	Enabled          bool
	EnabledByDefault bool
	Removed          bool
	UsedForAccess    bool
}

func (p Privilege) String() string {
	status := "Disabled"
	if p.Removed {
		status = "Removed"
	} else if p.Enabled {
		status = "Enabled"
	}
	return fmt.Sprintf("%s: %s", p.Name, status)
}

const (
	tokenUnknown tokenType = iota
	TokenPrimary
	TokenImpersonation
	TokenLinked
)

// NewToken can be used to supply your own token for the wintoken struct
// so you can use the same flexiblity provided by the package
func NewToken(token windows.Token, typ tokenType) *Token {
	return &Token{
		token: token,
		typ:   typ,
	}
}

// Token returns the underlying token for use
func (t *Token) Token() windows.Token {
	return t.token
}

// Close closes the underlying token
func (t *Token) Close() {
	windows.Close(windows.Handle(t.token))
	t.token = 0
}

func (t *Token) errIfTokenClosed() error {
	if t.token == 0 {
		return ErrTokenClosed
	}
	return nil
}

func lookupPrivilegeNameByLUID(luid uint64) (string, string, error) {
	nameBuffer := make([]uint16, 256)
	nameBufferSize := uint32(len(nameBuffer))
	displayNameBuffer := make([]uint16, 256)
	displayNameBufferSize := uint32(len(displayNameBuffer))

	sysName, err := windows.UTF16PtrFromString("")
	if err != nil {
		return "", "", err
	}

	if r1, _, err := procLookupPrivilegeName.Call(uintptr(unsafe.Pointer(sysName)), uintptr(unsafe.Pointer(&luid)), uintptr(unsafe.Pointer(&nameBuffer[0])), uintptr(unsafe.Pointer(&nameBufferSize))); r1 == 0 {
		return "", "", err
	}

	var langID uint32
	if r1, _, err := procLookupPrivilegeDisplayName.Call(uintptr(unsafe.Pointer(sysName)), uintptr(unsafe.Pointer(&nameBuffer[0])), uintptr(unsafe.Pointer(&displayNameBuffer[0])), uintptr(unsafe.Pointer(&displayNameBufferSize)), uintptr(unsafe.Pointer(&langID))); r1 == 0 {
		return "", "", err
	}

	return windows.UTF16ToString(nameBuffer), windows.UTF16ToString(displayNameBuffer), nil
}

// UserDetails gets User details associated with token
func (t *Token) UserDetails() (TokenUserDetail, error) {
	uSid, err := t.token.GetTokenUser()
	if err != nil {
		return TokenUserDetail{}, err
	}
	user, domain, typ, err := uSid.User.Sid.LookupAccount("")
	if err != nil {
		return TokenUserDetail{}, err
	}
	uProfDir, err := t.token.GetUserProfileDirectory()
	if err != nil {
		return TokenUserDetail{}, err
	}
	env, err := t.token.Environ(false)
	if err != nil {
		return TokenUserDetail{}, err
	}
	return TokenUserDetail{Username: user, Domain: domain, AccountType: typ, UserProfileDir: uProfDir, Environ: env}, nil
}

// GetPrivileges lists all Privileges from the token
func (t *Token) GetPrivileges() ([]Privilege, error) {
	if err := t.errIfTokenClosed(); err != nil {
		return nil, err
	}

	n := uint32(0)
	windows.GetTokenInformation(t.token, windows.TokenPrivileges, nil, 0, &n)

	b := make([]byte, n)
	if err := windows.GetTokenInformation(t.token, windows.TokenPrivileges, &b[0], uint32(len(b)), &n); err != nil {
		return nil, err
	}

	privBuff := bytes.NewBuffer(b)

	var nPrivs uint32
	if err := binary.Read(privBuff, binary.LittleEndian, &nPrivs); err != nil {
		return nil, fmt.Errorf("cannot read number of privileges: %w", err)
	}

	privDetails := make([]Privilege, int(nPrivs))

	for i := 0; i < int(nPrivs); i++ {

		var (
			luid            uint64
			attributes      uint32
			currentPrivInfo Privilege
			err             error
		)

		if err := binary.Read(privBuff, binary.LittleEndian, &luid); err != nil {
			return nil, fmt.Errorf("cannot read LUID from buffer: %w", err)
		}

		if err := binary.Read(privBuff, binary.LittleEndian, &attributes); err != nil {
			return nil, fmt.Errorf("cannot read attributes from buffer: %w", err)
		}

		currentPrivInfo.Name, currentPrivInfo.Description, err = lookupPrivilegeNameByLUID(luid)
		if err != nil {
			return nil, fmt.Errorf("cannot get privilege info based on the LUID: %w", err)
		}

		currentPrivInfo.EnabledByDefault = (attributes & windows.SE_PRIVILEGE_ENABLED_BY_DEFAULT) > 0
		currentPrivInfo.UsedForAccess = (attributes & windows.SE_PRIVILEGE_USED_FOR_ACCESS) > 0
		currentPrivInfo.Enabled = (attributes & windows.SE_PRIVILEGE_ENABLED) > 0
		currentPrivInfo.Removed = (attributes & windows.SE_PRIVILEGE_REMOVED) > 0

		privDetails[i] = currentPrivInfo
	}

	return privDetails, nil
}

// EnableAllPrivileges enables all privileges in the token
func (t *Token) EnableAllPrivileges() error {
	if err := t.errIfTokenClosed(); err != nil {
		return err
	}

	privs, err := t.GetPrivileges()
	if err != nil {
		return err
	}

	var toBeEnabled []string

	for _, p := range privs {
		if !p.Removed && !p.Enabled {
			toBeEnabled = append(toBeEnabled, p.Name)
		}
	}
	return t.modifyTokenPrivileges(toBeEnabled, PrivEnable)
}

// DisableAllPrivileges disables all privileges in the token
func (t *Token) DisableAllPrivileges() error {
	if err := t.errIfTokenClosed(); err != nil {
		return err
	}

	privs, err := t.GetPrivileges()
	if err != nil {
		return err
	}

	var toBeDisabled []string

	for _, p := range privs {
		if !p.Removed && p.Enabled {
			toBeDisabled = append(toBeDisabled, p.Name)
		}
	}
	return t.modifyTokenPrivileges(toBeDisabled, PrivDisable)
}

// RemoveAllPrivileges removes all privileges from the token
func (t *Token) RemoveAllPrivileges() error {
	if err := t.errIfTokenClosed(); err != nil {
		return err
	}

	privs, err := t.GetPrivileges()
	if err != nil {
		return err
	}

	var toBeRemoved []string

	for _, p := range privs {
		if !p.Removed {
			toBeRemoved = append(toBeRemoved, p.Name)
		}
	}
	return t.modifyTokenPrivileges(toBeRemoved, PrivRemove)
}

// EnableTokenPrivileges enables token privileges by list of privilege names
func (t *Token) EnableTokenPrivileges(privs []string) error {
	return t.modifyTokenPrivileges(privs, PrivEnable)
}

// DisableTokenPrivileges disables token privileges by list of privilege names
func (t *Token) DisableTokenPrivileges(privs []string) error {
	return t.modifyTokenPrivileges(privs, PrivDisable)
}

// RemoveTokenPrivileges removes token privileges by list of privilege names
func (t *Token) RemoveTokenPrivileges(privs []string) error {
	return t.modifyTokenPrivileges(privs, PrivRemove)
}

// EnableTokenPrivileges enables token privileges by privilege name
func (t *Token) EnableTokenPrivilege(priv string) error {
	return t.modifyTokenPrivilege(priv, PrivEnable)
}

// DisableTokenPrivilege disables token privileges by privilege name
func (t *Token) DisableTokenPrivilege(priv string) error {
	return t.modifyTokenPrivilege(priv, PrivDisable)
}

// RemoveTokenPrivilege removes token privileges by privilege name
func (t *Token) RemoveTokenPrivilege(priv string) error {
	return t.modifyTokenPrivilege(priv, PrivRemove)
}

func (t *Token) modifyTokenPrivileges(privs []string, mode privModType) error {
	if err := t.errIfTokenClosed(); err != nil {
		return err
	}

	if len(privs) == 0 {
		return ErrNoPrivilegesSpecified
	}

	errMsgConst := ""
	switch mode {
	case PrivDisable:
		errMsgConst = "disabling"
	case PrivEnable:
		errMsgConst = "enabling"
	case PrivRemove:
		errMsgConst = "removing"
	}
	var errMsg string
	for _, p := range privs {
		err := t.modifyTokenPrivilege(p, mode)
		if err != nil {
			if len(errMsg) != 0 {
				errMsg += "\n"
			}
			errMsg += fmt.Sprintf("%s privilege for %s failed: %s", errMsgConst, p, err)
		}
	}

	if len(errMsg) != 0 {
		return fmt.Errorf(errMsg)
	}
	return nil
}

func (t *Token) modifyTokenPrivilege(priv string, mode privModType) error {
	if err := t.errIfTokenClosed(); err != nil {
		return err
	}

	var luid windows.LUID

	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(priv), &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValueW failed: %w", err)
	}

	ap := windows.Tokenprivileges{
		PrivilegeCount: 1,
	}
	ap.Privileges[0].Luid = luid

	switch mode {
	case PrivEnable:
		ap.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	case PrivRemove:
		ap.Privileges[0].Attributes = windows.SE_PRIVILEGE_REMOVED
	}

	if err := windows.AdjustTokenPrivileges(t.token, false, &ap, 0, nil, nil); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges failed: %w", err)
	}

	return nil
}

// GetIntegrityLevel is used to get integrity level of the token
func (t *Token) GetIntegrityLevel() (string, error) {
	if err := t.errIfTokenClosed(); err != nil {
		return "", err
	}

	n := uint32(0)
	windows.GetTokenInformation(t.token, windows.TokenIntegrityLevel, nil, 0, &n)

	b := make([]byte, n)
	if err := windows.GetTokenInformation(t.token, windows.TokenIntegrityLevel, &b[0], uint32(len(b)), &n); err != nil {
		return "", err
	}

	tml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&b[0]))
	sid := (*windows.SID)(unsafe.Pointer(tml.Label.Sid))
	switch sid.String() {
	case "S-1-16-4096":
		return "Low", nil

	case "S-1-16-8192":
		return "Medium", nil

	case "S-1-16-12288":
		return "High", nil

	case "S-1-16-16384":
		return "System", nil

	default:
		return "Unknown", nil
	}
}

// GetLinkedToken is used to get the linked token if any
func (t *Token) GetLinkedToken() (*Token, error) {

	lt, err := t.token.GetLinkedToken()
	if err != nil {
		return nil, err
	}

	return &Token{
		typ:   TokenLinked,
		token: lt,
	}, nil
}
