//go:build ignore

// Build explicitly on Windows; this diagnostic is not an application package.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func filesystem(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	volume, fs := make([]uint16, 32768), make([]uint16, 256)
	if err := windows.GetVolumePathName(p, &volume[0], uint32(len(volume))); err != nil {
		return "", err
	}
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(fs), nil
}

func outsideCheckout(path, checkout string) (bool, error) {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return false, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false, err
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	actual, err = filepath.Abs(actual)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(actual)) {
		return true, nil
	}
	relative, err := filepath.Rel(root, actual)
	if err != nil {
		return false, err
	}
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func proof(storage string, desktop bool, checkout string) (map[string]any, error) {
	if err := windows.NewLazySystemDLL("ntdll.dll").NewProc("wine_get_version").Find(); err == nil {
		return nil, errors.New("Wine is not native Windows")
	}
	temp, err := os.MkdirTemp("", "makewand-go-runtime-proof-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	storageFS, err := filesystem(storage)
	if err != nil {
		return nil, err
	}
	tempFS, err := filesystem(temp)
	if err != nil {
		return nil, err
	}
	if storageFS != "NTFS" || tempFS != "NTFS" {
		return nil, errors.New("storage and actual Go MkdirTemp must both be NTFS")
	}
	v := windows.RtlGetVersion()
	result := map[string]any{"native_windows": true, "go_version": runtime.Version(), "go_arch": runtime.GOARCH,
		"windows_build": v.BuildNumber, "storage_filesystem": storageFS, "actual_go_temp_filesystem": tempFS}
	if checkout != "" {
		storageOutside, err := outsideCheckout(storage, checkout)
		if err != nil {
			return nil, err
		}
		tempOutside, err := outsideCheckout(temp, checkout)
		if err != nil {
			return nil, err
		}
		if !storageOutside || !tempOutside {
			return nil, errors.New("private storage and actual Go MkdirTemp must be outside the verification checkout")
		}
		result["storage_outside_checkout"] = true
		result["actual_go_temp_outside_checkout"] = true
	}
	if !desktop {
		return result, nil
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\MiniNT`, registry.QUERY_VALUE)
	if err == nil {
		k.Close()
		return nil, errors.New("WinPE does not certify a desktop")
	}
	if !errors.Is(err, registry.ErrNotExist) {
		return nil, fmt.Errorf("cannot determine WinPE capability: %w", err)
	}
	if v.ProductType != 1 {
		return nil, errors.New("desktop acceptance requires a Windows workstation")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	var elevated, size uint32
	if err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevated)), 4, &size); err != nil {
		return nil, err
	}
	if size != 4 || elevated != 0 || user.User.Sid.String() == "S-1-5-18" {
		return nil, errors.New("desktop acceptance requires a non-SYSTEM, non-elevated user token")
	}
	sidHash := sha256.Sum256([]byte(user.User.Sid.String()))
	result["user_sid_sha256"] = hex.EncodeToString(sidHash[:])
	result["non_system"] = true
	result["non_elevated"] = true
	result["non_winpe_workstation"] = true
	return result, nil
}

func main() {
	storage := flag.String("storage", ".", "existing verification storage directory")
	checkout := flag.String("outside-checkout", "", "require storage and actual Go temp outside this checkout")
	desktop := flag.Bool("desktop", false, "require an ordinary desktop user token")
	flag.Parse()
	result, err := proof(*storage, *desktop, *checkout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(1)
	}
}
