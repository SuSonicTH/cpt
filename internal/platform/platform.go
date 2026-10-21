// Package platform describes the OS/arch pairs cpt installs for.
package platform

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform is a target operating system and architecture.
type Platform struct {
	OS   string
	Arch string
}

// Current returns the platform the process is running on.
func Current() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// IsWindows reports whether the target is Windows.
func (p Platform) IsWindows() bool { return p.OS == "windows" }

// Parse accepts "os/arch" and normalises common aliases.
func Parse(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return Platform{}, fmt.Errorf("invalid platform %q: want os/arch", s)
	}
	osName, err := normalizeOS(parts[0])
	if err != nil {
		return Platform{}, err
	}
	arch, err := normalizeArch(parts[1])
	if err != nil {
		return Platform{}, err
	}
	return Platform{OS: osName, Arch: arch}, nil
}

func normalizeOS(s string) (string, error) {
	switch strings.ToLower(s) {
	case "linux":
		return "linux", nil
	case "darwin", "macos", "mac", "osx":
		return "darwin", nil
	case "windows", "win":
		return "windows", nil
	default:
		return "", fmt.Errorf("unsupported os %q", s)
	}
}

func normalizeArch(s string) (string, error) {
	switch strings.ToLower(s) {
	case "amd64", "x86_64", "x64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q", s)
	}
}

// Known reports whether the platform is one cpt supports.
func (p Platform) Known() bool {
	switch p.OS {
	case "linux", "darwin", "windows":
	default:
		return false
	}
	switch p.Arch {
	case "amd64", "arm64":
		return true
	}
	return false
}
