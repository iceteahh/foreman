package container

import (
	"os"
	"runtime"
	"strconv"
)

// UserForHost returns the --user value that keeps bind-mounted files writable
// by both the harness and the container. On Linux the container must run as
// the harness's own uid:gid (bind mounts preserve ownership). Docker Desktop
// on macOS and Windows maps ownership transparently, so the image user is kept.
func UserForHost() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}
