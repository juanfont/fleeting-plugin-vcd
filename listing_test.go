package vcd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
)

func TestReadListedVApps_StopsAtFirstFailure(t *testing.T) {
	g := &InstanceGroup{log: testLogger()}
	listed := []listedVApp{{"h1", "a"}, {"h2", "b"}, {"h3", "c"}, {"h4", "d"}}
	var readHREFs []string
	vApps, unread := g.readListedVApps(listed, func(href string) (*govcd.VApp, error) {
		readHREFs = append(readHREFs, href)
		switch href {
		case "h1":
			return &govcd.VApp{VApp: &types.VApp{HREF: href}}, nil
		case "h2":
			return nil, errors.New(vanishedVAppMsg)
		default:
			return nil, context.DeadlineExceeded
		}
	})
	assert.Len(t, vApps, 1)
	assert.Equal(t, []string{"h3", "h4"}, unread, "the failed vApp and everything after it are unread")
	assert.Equal(t, []string{"h1", "h2", "h3"}, readHREFs, "reads stop at the first failure")
}
