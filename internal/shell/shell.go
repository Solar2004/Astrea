// Package shell resolves how the agent runs a command line or a script file on
// the current OS: cmd or PowerShell on Windows, sh or bash on Linux, plus
// interpreters (python, node) for scripts.
package shell

import (
	"fmt"
	"runtime"
)

// Names lists the accepted shell/interpreter names, for JSON schema enums.
var Names = []string{"default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"}

var psFlags = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}

// Command returns the argv that runs a command line with the given shell.
// An empty name or "default" means cmd on Windows and sh elsewhere.
func Command(name, command string) (string, []string, error) {
	switch name {
	case "", "default":
		if runtime.GOOS == "windows" {
			return "cmd", []string{"/C", command}, nil
		}
		return "/bin/sh", []string{"-c", command}, nil
	case "cmd":
		return "cmd", []string{"/C", command}, nil
	case "powershell", "pwsh":
		return powershell(name), append(append([]string{}, psFlags...), "-Command", command), nil
	case "sh", "bash":
		return name, []string{"-c", command}, nil
	case "python":
		return python(), []string{"-c", command}, nil
	case "node":
		return "node", []string{"-e", command}, nil
	}
	return "", nil, fmt.Errorf("unknown shell %q (want one of %v)", name, Names)
}

// Script returns the argv that runs the script file at path, and the file
// extension the script must have for the interpreter to accept it.
func Script(name, path string) (string, []string, error) {
	switch name {
	case "", "default":
		if runtime.GOOS == "windows" {
			return Script("cmd", path)
		}
		return Script("sh", path)
	case "cmd":
		return "cmd", []string{"/C", path}, nil
	case "powershell", "pwsh":
		return powershell(name), append(append([]string{}, psFlags...), "-File", path), nil
	case "sh", "bash":
		return name, []string{path}, nil
	case "python":
		return python(), []string{path}, nil
	case "node":
		return "node", []string{path}, nil
	}
	return "", nil, fmt.Errorf("unknown shell %q (want one of %v)", name, Names)
}

// Ext returns the script file extension for a shell name.
func Ext(name string) string {
	switch name {
	case "", "default":
		if runtime.GOOS == "windows" {
			return ".cmd"
		}
		return ".sh"
	case "cmd":
		return ".cmd"
	case "powershell", "pwsh":
		return ".ps1"
	case "python":
		return ".py"
	case "node":
		return ".js"
	}
	return ".sh"
}

// powershell maps "powershell" to pwsh on non-Windows hosts, where only
// PowerShell 7 exists.
func powershell(name string) string {
	if name == "powershell" && runtime.GOOS != "windows" {
		return "pwsh"
	}
	return name
}

func python() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}
