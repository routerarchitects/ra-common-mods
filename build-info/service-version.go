package build_info

import (
	"fmt"
	"runtime/debug"
)

var version = ""
var buildTimestamp = ""
var commitHash = ""

func getVersion() string {
	return version
}

func GetBuildTimestamp() string {
	return buildTimestamp
}

func getCommitHash() string {
	return commitHash
}

func makeCommitHash() string {
	commitHash := getCommitHash()
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

func GetFullVersion() string {
	version := getVersion()

	if version == "" {
		return makeCommitHash()
	} else {
		return fmt.Sprintf("%s-%s", version, makeCommitHash())
	}

}
