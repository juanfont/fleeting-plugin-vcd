package vcd

import (
	"log"
	"runtime/debug"

	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
)

var (
	NAME      = "fleeting-plugin-vcd"
	VERSION   = "dev"
	REVISION  = "HEAD"
	REFERENCE = "HEAD"
	BUILT     = "now"

	Version plugin.VersionInfo
)

func init() {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		log.Fatal("failed to read build info")
	}

	Version = plugin.VersionInfo{
		Name:      NAME,
		Version:   buildInfo.Main.Version,
		Revision:  buildInfo.Main.Sum,
		Reference: buildInfo.Main.Path,
		BuiltAt:   buildInfo.Settings[0].Key,
	}
}
