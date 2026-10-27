//go:build windows

package main

// Launching the gui helper for a standard (non-administrator) account.
//
// The helper must run INSIDE the pupil's desktop session and AS the pupil (so
// whatever the AI opens belongs to that account and lives on their desktop),
// but with administrator rights available to the process — without ever adding
// the account to the Administrators group in the SAM, and without a password.
//
// The way to do that is an S4U (service-for-user) logon: as SYSTEM, with
// SeTcbPrivilege, we ask the local MSV1_0 authentication package for a token
// that represents the pupil but carries extra groups we choose. We inject:
//
//   - BUILTIN\Administrators (S-1-5-32-544), enabled: this is what gives the
//     process admin rights for the lifetime of the token only.
//   - the logon SID of the pupil's interactive session (SE_GROUP_LOGON_ID):
//     without it the process cannot open the window station / desktop
//     winsta0\default and CreateProcessAsUser fails with 0xC0000142.
//
// We then make the token a primary token, point it at the console session,
// raise its integrity to High (so it can drive elevated windows — UIPI would
// otherwise block input to a higher-integrity window), build the user's
// environment and start the helper on winsta0\default.
//
// Because S4U needs a trusted logon process to inject groups, we register one
// with LsaRegisterLogonProcess (requires SeTcbPrivilege; SYSTEM has it). If any
// of that fails we fall back to the pupil's plain interactive token
// (WTSQueryUserToken): the helper still works, just without admin rights.
//
// What this does NOT reach: the UAC secure desktop (consent.exe) runs on its
// own desktop that even a High-integrity user process cannot touch; and this is
// not Winlogon (that is the unlock path). RISK, accepted for classroom PCs:
// anything the AI opens in this session runs with administrator rights while
// the pupil is sitting in front of it; the pupil could use those windows.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	secur32              = windows.NewLazySystemDLL("secur32.dll")
	pLsaRegisterLogon    = secur32.NewProc("LsaRegisterLogonProcess")
	pLsaDeregisterLogon  = secur32.NewProc("LsaDeregisterLogonProcess")
	pLsaLookupAuthPkg    = secur32.NewProc("LsaLookupAuthenticationPackage")
	pLsaLogonUser        = secur32.NewProc("LsaLogonUser")
	pLsaFreeReturnBuffer = secur32.NewProc("LsaFreeReturnBuffer")

	advapi32               = windows.NewLazySystemDLL("advapi32.dll")
	pAllocateLocallyUnique = advapi32.NewProc("AllocateLocallyUniqueId")
)

// lsaString is an LSA_STRING: a counted ANSI string.
type lsaString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *byte
}

// newLSAString keeps the backing bytes alive via the returned slice; the
// caller must hold on to it for as long as the lsaString is used.
func newLSAString(s string) (lsaString, []byte) {
	b := append([]byte(s), 0)
	return lsaString{
		Length:        uint16(len(s)),
		MaximumLength: uint16(len(s) + 1),
		Buffer:        &b[0],
	}, b
}

// unicodeString is a UNICODE_STRING whose Buffer must point inside the same
// blob passed to LsaLogonUser (LSA relocates such embedded pointers).
type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

// msvS4ULogon is MSV1_0_S4U_LOGON. MessageType is MsV1_0S4ULogon.
type msvS4ULogon struct {
	MessageType       uint32
	Flags             uint32
	UserPrincipalName unicodeString
	DomainName        unicodeString
}

const msV10S4ULogon = 12 // MSV1_0_LOGON_SUBMIT_TYPE::MsV1_0S4ULogon

// tokenSource is TOKEN_SOURCE.
type tokenSource struct {
	SourceName       [8]byte
	SourceIdentifier windows.LUID
}

// quotaLimits is QUOTA_LIMITS (output only; we never read it).
type quotaLimits struct {
	PagedPoolLimit        uintptr
	NonPagedPoolLimit     uintptr
	MinimumWorkingSetSize uintptr
	MaximumWorkingSetSize uintptr
	PagefileLimit         uintptr
	TimeLimit             int64
}

// tokenGroups2 is a TOKEN_GROUPS holding exactly the two groups we inject.
type tokenGroups2 struct {
	GroupCount uint32
	Groups     [2]windows.SIDAndAttributes
}

// tokenMandatoryLabel is TOKEN_MANDATORY_LABEL (for the integrity level).
type tokenMandatoryLabel struct {
	Label windows.SIDAndAttributes
}

const logonTypeNetwork = 3 // SECURITY_LOGON_TYPE::Network (the type S4U uses)

// launchConsoleHelper starts the gui helper in the console session for the
// standard user signed in there, with the per-launch secret in its environment.
// It tries the S4U-with-admin token first and falls back to the plain user
// token, logging why, so screen/mouse/keyboard still come up either way.
func launchConsoleHelper(secret, guiAddr string, logger *log.Logger) error {
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return fmt.Errorf("no active console session")
	}
	var userTok windows.Token
	if err := windows.WTSQueryUserToken(session, &userTok); err != nil {
		return fmt.Errorf("WTSQueryUserToken(session %d): %w", session, err)
	}
	defer userTok.Close()

	src, admin, err := s4uAdminToken(userTok, session, logger)
	if err != nil {
		logger.Printf("gui helper: no admin rights for the helper (S4U failed: %v); launching with the plain user token", err)
		src, admin = userTok, false
	} else {
		defer src.Close()
	}
	return spawnHelper(src, session, secret, guiAddr, admin, logger)
}

// s4uAdminToken returns a primary token that represents the console user but
// carries Administrators (enabled) and the session's logon SID, at High
// integrity. The returned token is the caller's to close.
func s4uAdminToken(userTok windows.Token, session uint32, logger *log.Logger) (windows.Token, bool, error) {
	logon, err := sessionLogonSID(userTok)
	if err != nil {
		return 0, false, fmt.Errorf("logon SID: %w", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, false, err
	}
	user, domain := consoleUser(), consoleDomain()
	if user == "" {
		return 0, false, fmt.Errorf("no console user name")
	}
	if domain == "" {
		if h, err := os.Hostname(); err == nil {
			domain = h
		}
	}

	// Trusted connection to LSA (needed to inject groups).
	name, nameBuf := newLSAString("Astrea")
	_ = nameBuf
	var lsa windows.Handle
	var mode uint32
	if st, _, _ := pLsaRegisterLogon.Call(uintptr(unsafe.Pointer(&name)),
		uintptr(unsafe.Pointer(&lsa)), uintptr(unsafe.Pointer(&mode))); st != 0 {
		return 0, false, fmt.Errorf("LsaRegisterLogonProcess: %s", ntStatus(st))
	}
	defer pLsaDeregisterLogon.Call(uintptr(lsa))

	pkgName, pkgBuf := newLSAString("MICROSOFT_AUTHENTICATION_PACKAGE_V1_0")
	_ = pkgBuf
	var authPkg uint32
	if st, _, _ := pLsaLookupAuthPkg.Call(uintptr(lsa),
		uintptr(unsafe.Pointer(&pkgName)), uintptr(unsafe.Pointer(&authPkg))); st != 0 {
		return 0, false, fmt.Errorf("LsaLookupAuthenticationPackage: %s", ntStatus(st))
	}

	// One contiguous MSV1_0_S4U_LOGON buffer: the struct, then the two strings
	// it points at (LSA relocates the embedded pointers).
	userW := utf16.Encode([]rune(user))
	domW := utf16.Encode([]rune(domain))
	hdr := int(unsafe.Sizeof(msvS4ULogon{}))
	blob := make([]byte, hdr+len(userW)*2+len(domW)*2)
	msv := (*msvS4ULogon)(unsafe.Pointer(&blob[0]))
	msv.MessageType = msV10S4ULogon
	userOff, domOff := hdr, hdr+len(userW)*2
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&blob[userOff])), len(userW)), userW)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&blob[domOff])), len(domW)), domW)
	msv.UserPrincipalName = unicodeString{Length: uint16(len(userW) * 2), MaximumLength: uint16(len(userW) * 2),
		Buffer: (*uint16)(unsafe.Pointer(&blob[userOff]))}
	msv.DomainName = unicodeString{Length: uint16(len(domW) * 2), MaximumLength: uint16(len(domW) * 2),
		Buffer: (*uint16)(unsafe.Pointer(&blob[domOff]))}

	groups := tokenGroups2{GroupCount: 2}
	groups.Groups[0] = windows.SIDAndAttributes{Sid: admins,
		Attributes: windows.SE_GROUP_ENABLED | windows.SE_GROUP_ENABLED_BY_DEFAULT | windows.SE_GROUP_MANDATORY}
	groups.Groups[1] = windows.SIDAndAttributes{Sid: logon,
		Attributes: windows.SE_GROUP_LOGON_ID | windows.SE_GROUP_ENABLED | windows.SE_GROUP_ENABLED_BY_DEFAULT | windows.SE_GROUP_MANDATORY}

	origin, originBuf := newLSAString("Astrea")
	_ = originBuf
	var source tokenSource
	copy(source.SourceName[:], "Astrea")
	pAllocateLocallyUnique.Call(uintptr(unsafe.Pointer(&source.SourceIdentifier)))

	var profile uintptr
	var profileLen uint32
	var logonID windows.LUID
	var token windows.Token
	var quotas quotaLimits
	var subStatus int32

	st, _, _ := pLsaLogonUser.Call(
		uintptr(lsa),
		uintptr(unsafe.Pointer(&origin)),
		uintptr(logonTypeNetwork),
		uintptr(authPkg),
		uintptr(unsafe.Pointer(&blob[0])),
		uintptr(len(blob)),
		uintptr(unsafe.Pointer(&groups)),
		uintptr(unsafe.Pointer(&source)),
		uintptr(unsafe.Pointer(&profile)),
		uintptr(unsafe.Pointer(&profileLen)),
		uintptr(unsafe.Pointer(&logonID)),
		uintptr(unsafe.Pointer(&token)),
		uintptr(unsafe.Pointer(&quotas)),
		uintptr(unsafe.Pointer(&subStatus)),
	)
	if profile != 0 {
		pLsaFreeReturnBuffer.Call(profile)
	}
	if st != 0 {
		return 0, false, fmt.Errorf("LsaLogonUser: %s (substatus %s)", ntStatus(st), ntStatus(uintptr(uint32(subStatus))))
	}

	// High integrity, so the helper can drive elevated windows (UIPI).
	if err := setHighIntegrity(token); err != nil {
		logger.Printf("gui helper: could not raise the helper token to High integrity: %v (it still has admin rights, medium integrity)", err)
	}
	return token, true, nil
}

// spawnHelper duplicates src to a primary token in the console session and
// starts the gui helper on winsta0\default with the secret in its environment.
func spawnHelper(src windows.Token, session uint32, secret, guiAddr string, admin bool, logger *log.Logger) error {
	var primary windows.Token
	if err := windows.DuplicateTokenEx(src, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primary.Close()

	sid := session
	if err := windows.SetTokenInformation(primary, windows.TokenSessionId,
		(*byte)(unsafe.Pointer(&sid)), uint32(unsafe.Sizeof(sid))); err != nil {
		return fmt.Errorf("SetTokenInformation(session): %w", err)
	}

	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primary, false); err != nil {
		return fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(envBlock)
	env := mergeEnv(envBlock, "ASTREA_GUI_SECRET="+secret)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	cmdLine := fmt.Sprintf(`"%s" --role gui --gui-addr %s`, exe, guiAddr)
	cmd16, _ := windows.UTF16PtrFromString(cmdLine)
	dir16, _ := windows.UTF16PtrFromString(dir)

	si := &windows.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Desktop, _ = windows.UTF16PtrFromString(`winsta0\default`)
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(primary, nil, cmd16, nil, nil, false,
		windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, &env[0], dir16, si, &pi); err != nil {
		return fmt.Errorf("CreateProcessAsUser(winsta0\\default): %w", err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	logger.Printf("gui helper: started pid=%d in console session %d (admin rights: %v)", pi.ProcessId, session, admin)
	return nil
}

// sessionLogonSID extracts the logon SID of tok's interactive session.
func sessionLogonSID(tok windows.Token) (*windows.SID, error) {
	var n uint32
	windows.GetTokenInformation(tok, windows.TokenLogonSid, nil, 0, &n)
	if n == 0 {
		return nil, fmt.Errorf("TokenLogonSid returned size 0")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenLogonSid, &buf[0], n, &n); err != nil {
		return nil, err
	}
	tg := (*windows.Tokengroups)(unsafe.Pointer(&buf[0]))
	g := tg.AllGroups()
	if len(g) == 0 {
		return nil, fmt.Errorf("no logon SID in token")
	}
	return g[0].Sid.Copy() // out of buf, which is about to be freed
}

// setHighIntegrity sets tok's integrity level to High (S-1-16-12288).
func setHighIntegrity(tok windows.Token) error {
	high, err := windows.StringToSid("S-1-16-12288")
	if err != nil {
		return err
	}
	label := tokenMandatoryLabel{Label: windows.SIDAndAttributes{Sid: high, Attributes: windows.SE_GROUP_INTEGRITY}}
	return windows.SetTokenInformation(tok, windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&label)), uint32(unsafe.Sizeof(label)))
}

// mergeEnv copies the CreateEnvironmentBlock double-null-terminated block and
// appends extra "KEY=VALUE" entries, returning a fresh, terminated block.
func mergeEnv(block *uint16, extra ...string) []uint16 {
	var env []uint16
	for p := block; p != nil; {
		start := p
		n := 0
		for *(*uint16)(unsafe.Add(unsafe.Pointer(start), uintptr(n)*2)) != 0 {
			n++
		}
		if n == 0 {
			break // empty string: end of the block
		}
		env = append(env, unsafe.Slice(start, n)...)
		env = append(env, 0)
		p = (*uint16)(unsafe.Add(unsafe.Pointer(start), uintptr(n+1)*2))
	}
	for _, e := range extra {
		env = append(env, utf16.Encode([]rune(e))...)
		env = append(env, 0)
	}
	env = append(env, 0) // final terminator
	return env
}

// consoleDomain names the domain of the console session's user (the machine
// name for a local account), "" if it cannot be read.
func consoleDomain() string {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xFFFFFFFF {
		return ""
	}
	const wtsDomainName = 7
	var buf *uint16
	var n uint32
	r, _, _ := procWTSQuerySessionInformation.Call(0, uintptr(id), wtsDomainName,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf)
}

// ntStatus renders an NTSTATUS for a log line.
func ntStatus(st uintptr) string { return fmt.Sprintf("0x%08X", uint32(st)) }
