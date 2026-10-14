// Package deps detects and installs the tooling an AI workload needs on the
// controlled machine (git, python, CUDA, …) so the agent can bootstrap a
// fresh PC by itself: winget on Windows, apt/pacman/dnf on Linux.
//
// Detection never trusts the installer's exit code alone: after installing,
// PATH is re-read from the system and the dependency is checked again.
package deps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
)

// Dep is one known dependency and how to detect and install it per platform.
type Dep struct {
	Name        string
	Description string
	// Bins are executables that prove the dependency is present; the first
	// one found on PATH wins.
	Bins []string
	// Files are absolute paths (%VAR% or $VAR expanded). With Bins they are
	// fallback locations for a binary not on PATH; without Bins they describe
	// a library (VC++ runtime) and must all exist.
	Files []string
	// VersionArgs are passed to the found binary to report its version.
	VersionArgs []string
	// Package ids per manager. Empty means "not installable that way".
	Winget, Apt, Pacman, Dnf string
	// WingetArgs are extra flags for winget install (e.g. --override).
	WingetArgs []string
	// Manual explains what to do when the agent cannot install it itself.
	Manual string
	// OS restricts the dependency to one GOOS ("" = any).
	OS string
}

// Catalog is the set of dependencies the agent knows by name.
var Catalog = []Dep{
	{Name: "git", Description: "Git", Bins: []string{"git"}, VersionArgs: []string{"--version"},
		Winget: "Git.Git", Apt: "git", Pacman: "git", Dnf: "git"},
	{Name: "git-lfs", Description: "Git LFS (large model repos on HuggingFace)", Bins: []string{"git-lfs"}, VersionArgs: []string{"--version"},
		Winget: "GitHub.GitLFS", Apt: "git-lfs", Pacman: "git-lfs", Dnf: "git-lfs"},
	{Name: "python", Description: "Python 3.12", Bins: []string{"python", "python3", "py"}, VersionArgs: []string{"--version"},
		Winget: "Python.Python.3.12", Apt: "python3 python3-venv python3-pip", Pacman: "python python-pip", Dnf: "python3 python3-pip"},
	{Name: "uv", Description: "uv (fast Python/venv manager, recommended for ComfyUI and friends)", Bins: []string{"uv"}, VersionArgs: []string{"--version"},
		Winget: "astral-sh.uv", Pacman: "uv",
		Manual: "curl -LsSf https://astral.sh/uv/install.sh | sh"},
	{Name: "7zip", Description: "7-Zip", Bins: []string{"7z", "7za"}, Files: []string{`%ProgramFiles%\7-Zip\7z.exe`},
		Winget: "7zip.7zip", Apt: "p7zip-full", Pacman: "7zip", Dnf: "p7zip"},
	{Name: "vcredist", Description: "Microsoft Visual C++ 2015+ runtime (x64)", OS: "windows",
		Files:  []string{`%SystemRoot%\System32\vcruntime140.dll`, `%SystemRoot%\System32\msvcp140.dll`},
		Winget: "Microsoft.VCRedist.2015+.x64"},
	{Name: "nvidia-driver", Description: "NVIDIA GPU driver (needed for any CUDA workload)", Bins: []string{"nvidia-smi"},
		VersionArgs: []string{"--query-gpu=driver_version,name", "--format=csv,noheader"},
		Files:       []string{`%SystemRoot%\System32\nvidia-smi.exe`, `%ProgramFiles%\NVIDIA Corporation\NVSMI\nvidia-smi.exe`},
		Manual:      "Install the driver from nvidia.com or the NVIDIA App; it usually needs a reboot. llama.cpp CUDA builds and PyTorch wheels bundle the CUDA runtime, so the driver alone is enough for them."},
	{Name: "cuda", Description: "CUDA toolkit (nvcc) — only needed to COMPILE CUDA code", Bins: []string{"nvcc"}, VersionArgs: []string{"--version"},
		Winget: "Nvidia.CUDA", Apt: "nvidia-cuda-toolkit", Pacman: "cuda", Dnf: "cuda"},
	{Name: "ffmpeg", Description: "FFmpeg", Bins: []string{"ffmpeg"}, VersionArgs: []string{"-version"},
		Winget: "Gyan.FFmpeg", Apt: "ffmpeg", Pacman: "ffmpeg", Dnf: "ffmpeg"},
	{Name: "node", Description: "Node.js LTS", Bins: []string{"node"}, VersionArgs: []string{"--version"},
		Winget: "OpenJS.NodeJS.LTS", Apt: "nodejs npm", Pacman: "nodejs npm", Dnf: "nodejs npm"},
	{Name: "cmake", Description: "CMake", Bins: []string{"cmake"}, VersionArgs: []string{"--version"},
		Winget: "Kitware.CMake", Apt: "cmake", Pacman: "cmake", Dnf: "cmake"},
	{Name: "llama.cpp", Description: "llama.cpp (llama-server, OpenAI-compatible local LLM server)", Bins: []string{"llama-server"}, VersionArgs: []string{"--version"},
		Winget: "ggml.llamacpp",
		Manual: "On Linux download a release from https://github.com/ggml-org/llama.cpp/releases or build it."},
	{Name: "ollama", Description: "Ollama", Bins: []string{"ollama"}, VersionArgs: []string{"--version"},
		Winget: "Ollama.Ollama",
		Manual: "curl -fsSL https://ollama.com/install.sh | sh"},
}

// Lookup returns the catalog entry for name, or a synthetic Dep for a raw
// package spec "manager:package" (e.g. "winget:Mozilla.Firefox", "apt:htop").
func Lookup(name string) (Dep, error) {
	for _, d := range Catalog {
		if strings.EqualFold(d.Name, name) {
			return d, nil
		}
	}
	mgr, pkg, ok := strings.Cut(name, ":")
	if ok && pkg != "" {
		d := Dep{Name: name, Description: "raw " + mgr + " package " + pkg}
		switch mgr {
		case "winget":
			d.Winget = pkg
		case "apt":
			d.Apt = pkg
		case "pacman":
			d.Pacman = pkg
		case "dnf":
			d.Dnf = pkg
		default:
			return Dep{}, fmt.Errorf("unknown package manager %q (want winget|apt|pacman|dnf)", mgr)
		}
		return d, nil
	}
	return Dep{}, fmt.Errorf("unknown dependency %q (known: %s; or use manager:package)", name, strings.Join(Names(), ", "))
}

// Names lists the catalog names, sorted.
func Names() []string {
	out := make([]string, 0, len(Catalog))
	for _, d := range Catalog {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// Status is the detection result for one dependency.
type Status struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Present     bool   `json:"present"`
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	// Installer is how the agent would install it here ("winget Git.Git"),
	// empty when it cannot.
	Installer string `json:"installer,omitempty"`
	Manual    string `json:"manual,omitempty"`
	// Result is set by Ensure: present | installed | failed | manual | unsupported.
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// raw reports whether d is a "manager:package" spec, which cannot be detected
// by binary and is always (re)installed by Ensure.
func (d Dep) raw() bool { return len(d.Bins) == 0 && len(d.Files) == 0 }

// Check detects d on this machine.
func Check(ctx context.Context, d Dep) Status {
	st := Status{Name: d.Name, Description: d.Description, Manual: d.Manual}
	if inst, err := installer(d); err == nil {
		st.Installer = strings.Join(inst.display, " ")
	}
	if d.OS != "" && d.OS != runtime.GOOS {
		st.Result = "unsupported"
		return st
	}
	for _, b := range d.Bins {
		if p, err := lookPath(b); err == nil {
			st.Present, st.Path = true, p
			break
		}
	}
	if !st.Present && len(d.Files) > 0 {
		// With Bins, Files are fallback locations (any one will do); without
		// Bins they describe a library, and every file must exist.
		fallback := len(d.Bins) > 0
		found := ""
		for _, f := range d.Files {
			p := filepath.FromSlash(os.ExpandEnv(expandWinVars(f)))
			fi, err := os.Stat(p)
			ok := err == nil && !fi.IsDir()
			if ok && found == "" {
				found = p
			}
			if ok == fallback {
				if !fallback {
					found = ""
				}
				break
			}
		}
		st.Present, st.Path = found != "", found
	}
	if st.Present && len(d.VersionArgs) > 0 && st.Path != "" && !strings.HasSuffix(strings.ToLower(st.Path), ".dll") {
		st.Version = version(ctx, st.Path, d.VersionArgs)
	}
	return st
}

// expandWinVars turns %VAR% into ${VAR} so os.ExpandEnv handles both styles.
// Names with special characters (ProgramFiles(x86)) are not supported.
func expandWinVars(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			break
		}
		b.WriteString(s[:i])
		b.WriteString("${" + s[i+1:i+1+j] + "}")
		s = s[i+2+j:]
	}
	b.WriteString(s)
	return b.String()
}

func version(ctx context.Context, bin string, args []string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	// nvcc and cmake print several lines; keep the most informative one.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(strings.ToLower(l), "release") {
			return l
		}
	}
	return strings.TrimSpace(lines[0])
}

// installCmd is a resolved install command line.
type installCmd struct {
	argv    []string
	display []string
	manager string
}

var errNoInstaller = errors.New("no installer for this platform")

func installer(d Dep) (installCmd, error) {
	if runtime.GOOS == "windows" {
		if d.Winget == "" {
			return installCmd{}, errNoInstaller
		}
		argv := []string{"winget", "install", "--id", d.Winget, "--exact", "--silent",
			"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"}
		argv = append(argv, d.WingetArgs...)
		return installCmd{argv: argv, display: []string{"winget", d.Winget}, manager: "winget"}, nil
	}
	type mgr struct {
		bin, pkgs string
		argv      []string
	}
	for _, m := range []mgr{
		{"apt-get", d.Apt, []string{"apt-get", "install", "-y", "--no-install-recommends"}},
		{"pacman", d.Pacman, []string{"pacman", "-S", "--noconfirm", "--needed"}},
		{"dnf", d.Dnf, []string{"dnf", "install", "-y"}},
	} {
		if _, err := exec.LookPath(m.bin); err != nil {
			continue
		}
		if m.pkgs == "" {
			return installCmd{}, errNoInstaller
		}
		argv := append(m.argv, strings.Fields(m.pkgs)...)
		if os.Geteuid() != 0 {
			// Non-interactive sudo: fails fast instead of hanging on a password.
			argv = append([]string{"sudo", "-n"}, argv...)
		}
		return installCmd{argv: argv, display: []string{m.bin, m.pkgs}, manager: m.bin}, nil
	}
	return installCmd{}, errNoInstaller
}

// Emit receives installer output line by line.
type Emit func(channel, line string)

// installMu serialises installs: winget and apt both hold global locks and
// fail if two run at once.
var installMu sync.Mutex

// Ensure makes sure d is present, installing it if needed.
func Ensure(ctx context.Context, d Dep, emit Emit) Status {
	st := Check(ctx, d)
	if st.Result == "unsupported" {
		return st
	}
	if st.Present && !d.raw() {
		st.Result = "present"
		return st
	}
	inst, err := installer(d)
	if err != nil {
		st.Result = "manual"
		if st.Manual == "" {
			st.Error = err.Error()
		}
		return st
	}

	installMu.Lock()
	defer installMu.Unlock()
	if inst.manager == "apt-get" {
		_ = run(ctx, aptUpdate(), emit) // stale indexes are the usual apt failure
	}
	runErr := run(ctx, inst.argv, emit)
	RefreshPath()

	after := Check(ctx, d)
	after.Result = "installed"
	if d.raw() {
		if runErr != nil && !alreadyInstalled(runErr) {
			after.Result, after.Error = "failed", runErr.Error()
		}
		return after
	}
	if !after.Present {
		after.Result = "failed"
		if runErr != nil {
			after.Error = runErr.Error()
		} else {
			after.Error = "installer finished but the dependency is still not detected (a reboot or new login may be required)"
		}
	}
	return after
}

func aptUpdate() []string {
	argv := []string{"apt-get", "update"}
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	return argv
}

// winget exit codes that mean "nothing to do".
const (
	wingetNoUpdate         = -1978335189 // 0x8A15002B APPINSTALLER_CLI_ERROR_UPDATE_NOT_APPLICABLE
	wingetAlreadyInstalled = -1978335135 // 0x8A150061 APPINSTALLER_CLI_ERROR_PACKAGE_ALREADY_INSTALLED
)

func alreadyInstalled(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	c := int32(ee.ExitCode())
	return c == wingetNoUpdate || c == wingetAlreadyInstalled
}

func run(ctx context.Context, argv []string, emit Emit) error {
	bin, err := lookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s not found: %w", argv[0], err)
	}
	if emit != nil {
		emit("stdout", "$ "+strings.Join(argv, " ")+"\n")
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	procutil.NoConsole(cmd)
	if runtime.GOOS != "windows" {
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, stdout, "stdout", emit)
	go pipe(&wg, stderr, "stderr", emit)
	wg.Wait()
	return cmd.Wait()
}

func pipe(wg *sync.WaitGroup, r io.Reader, channel string, emit Emit) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	// winget draws progress bars with \r; split on it too so they don't pile
	// up into one giant line.
	sc.Split(scanLinesCR)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || emit == nil || isSpinner(line) {
			continue
		}
		emit(channel, line+"\n")
	}
}

func scanLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		if c == '\n' || c == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// isSpinner drops winget's "-", "\", "|", "/" spinner frames and bare
// progress-bar redraws, which otherwise flood the stream.
func isSpinner(line string) bool {
	if len(line) == 1 && strings.ContainsAny(line, `-\|/`) {
		return true
	}
	return strings.ContainsRune(line, '█') || strings.ContainsRune(line, '▒')
}
