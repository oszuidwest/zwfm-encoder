package util

import (
	"os"
	"path/filepath"
)

// windowsAppDirName is the encoder's directory name under the Windows data
// roots (%PROGRAMDATA%, %LOCALAPPDATA%, temp).
const windowsAppDirName = "encoder"

// WindowsDataDir returns the machine-wide encoder data directory on Windows:
// %PROGRAMDATA%\encoder, defaulting to C:\ProgramData when the environment
// variable is unset. It is the single owner of that derivation; the event
// log and the slog file both build their paths from it. The function has no
// build tag so runtime.GOOS switches can call it on every platform, but the
// result is only meaningful on Windows.
func WindowsDataDir() string {
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, windowsAppDirName)
}
