package vcd

import (
	"fmt"
	"log"
	"runtime/debug"

	"github.com/davecgh/go-spew/spew"
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

	spew.Dump(buildInfo)

	Version = plugin.VersionInfo{
		Name:      NAME,
		Version:   buildInfo.Main.Version,
		Revision:  buildInfo.Main.Sum,
		Reference: buildInfo.Main.Path,
		BuiltAt:   buildInfo.Settings[0].Key,
	}

	fmt.Println(Version.String())
	fmt.Println(Version.BuildInfo())
}
