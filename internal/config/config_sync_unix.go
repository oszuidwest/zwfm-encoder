//go:build !windows

package config

import (
	"os"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// syncDirPath fsyncs the directory so the renamed directory entry is durable.
func syncDirPath(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // G304: dir is derived from the config file path set at startup
	if err != nil {
		return util.WrapError("open config directory", err)
	}
	defer func() {
		_ = f.Close()
	}()

	if err := f.Sync(); err != nil {
		return util.WrapError("sync config directory", err)
	}
	if err := f.Close(); err != nil {
		return util.WrapError("close config directory", err)
	}

	return nil
}
