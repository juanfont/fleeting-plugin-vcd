package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	vcd "github.com/juanfont/fleeting-plugin-vcd"
)

func main() {
	plugin.Main(&vcd.InstanceGroup{}, vcd.Version)
}
