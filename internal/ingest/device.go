package ingest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
)

// DeviceFingerprint captures anonymous device information for session continuity checks.
type DeviceFingerprint struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	HostnameHash  string `json:"hostnameHash"`
	UsernameHash  string `json:"usernameHash"`
	MachineIDHash string `json:"machineIdHash"`
	Shell         string `json:"shell"`
	CPUCount      int    `json:"cpuCount"`
	RAMBytes      uint64 `json:"ramBytes"`
}

func CollectDeviceFingerprint() DeviceFingerprint {
	fp := DeviceFingerprint{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCount: runtime.NumCPU(),
		RAMBytes: totalRAM(),
		Shell:    filepath_Base(os.Getenv("SHELL")),
	}

	if hostname, err := os.Hostname(); err == nil {
		fp.HostnameHash = Sha256Hex(hostname)
	}
	if u, err := user.Current(); err == nil {
		fp.UsernameHash = Sha256Hex(u.Username)
	}
	if mid := readMachineID(); mid != "" {
		fp.MachineIDHash = Sha256Hex(mid)
	}

	return fp
}

func Sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// filepath_Base returns the last element of path without importing filepath again.
func filepath_Base(path string) string {
	if path == "" {
		return ""
	}
	i := len(path) - 1
	for i >= 0 && path[i] != '/' {
		i--
	}
	return path[i+1:]
}

// readMachineID returns a platform-specific machine identifier.
func readMachineID() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "IOPlatformUUID") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					return strings.Trim(strings.TrimSpace(parts[1]), "\"")
				}
			}
		}
		return ""
	case "linux":
		data, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	default:
		return ""
	}
}

// totalRAM returns total system memory in bytes.
func totalRAM() uint64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		val, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0
		}
		return val
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, err := strconv.ParseUint(fields[1], 10, 64)
					if err != nil {
						return 0
					}
					return kb * 1024
				}
			}
		}
		return 0
	default:
		return 0
	}
}
