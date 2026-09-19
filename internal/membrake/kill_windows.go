//go:build windows

package membrake

import "errors"

func killGroup(pgid int) error { return errors.New("membrake: unsupported on windows") }

func groupAlive(pgid int) bool { return false }
