package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const windowsRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

const windowsRunValueName = "Koushin"

func setWindowsRunOnStartup(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !enabled {
		err := k.DeleteValue(windowsRunValueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	cmd := "\"" + exe + "\""
	return k.SetStringValue(windowsRunValueName, cmd)
}

func startMenuShortcutPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = strings.TrimSpace(os.Getenv("APPDATA"))
	}
	if base == "" {
		return "", errors.New("could not determine AppData directory")
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", "Koushin", "Koushin.lnk"), nil
}

func escapePowerShellSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func ensureStartMenuShortcut() error {
	lnkPath, err := startMenuShortcutPath()
	if err != nil {
		return err
	}
	if fileExists(lnkPath) {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	wd := filepath.Dir(exe)

	if err := os.MkdirAll(filepath.Dir(lnkPath), 0700); err != nil {
		return err
	}

	ps := "$WshShell = New-Object -ComObject WScript.Shell; " +
		"$Shortcut = $WshShell.CreateShortcut('" + escapePowerShellSingleQuoted(lnkPath) + "'); " +
		"$Shortcut.TargetPath = '" + escapePowerShellSingleQuoted(exe) + "'; " +
		"$Shortcut.WorkingDirectory = '" + escapePowerShellSingleQuoted(wd) + "'; " +
		"$Shortcut.IconLocation = '" + escapePowerShellSingleQuoted(exe) + ",0'; " +
		"$Shortcut.Description = 'Koushin'; " +
		"$Shortcut.Save();"

	cmd := hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ps)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create start menu shortcut: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func openBrowser(u string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK           = 0x00000000
	mbOKCancel     = 0x00000001
	mbYesNo        = 0x00000004
	mbIconInfo     = 0x00000040
	mbIconQuestion = 0x00000020
	mbIconError    = 0x00000010

	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	idOK  = 1
	idYes = 6
)

func messageBox(title, text string, style uint32) int {
	style |= mbSetForeground | mbTopMost

	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(t)),
		uintptr(unsafe.Pointer(c)),
		uintptr(style),
	)
	return int(r)
}

// hiddenCommand is exec.Command for console programs, without flashing a
// console window (Koushin itself is a GUI app).
func hiddenCommand(name string, args ...string) *exec.Cmd {
	const createNoWindow = 0x08000000
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}
