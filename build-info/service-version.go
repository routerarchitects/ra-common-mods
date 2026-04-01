package build_info

import (
	"runtime/debug"
)

var version = ""
var buildTimestamp = ""
var commitHash = ""

func GetVersion() string {
	return version
}

func GetBuildTimestamp() string {
	return buildTimestamp
}

func GetCommitHash() string {
	if commitHash == "" {
		info, ok := debug.ReadBuildInfo()
		if ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					commitHash = s.Value
				}
			}
		}
	}
	return commitHash
}
