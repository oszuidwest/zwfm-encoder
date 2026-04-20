//go:build windows

package config

// syncDirPath is a no-op on Windows: MoveFileExW does not provide the same
// directory-entry durability guarantee, so there is nothing to fsync here.
func syncDirPath(_ string) error {
	return nil
}
