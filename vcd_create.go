package vcd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
)

func (g *InstanceGroup) createRawVApp(ctx context.Context, client *govcd.VCDClient, vdc *govcd.Vdc, name, description string) (*govcd.VApp, error) {
	// The SDK's CreateRawVApp combines POST and polling. Split them so a failed
	// read can never cause another POST and task polls can renew authentication.
	vapp, createErr := singleVCDCall(ctx, g, "CreateRawVApp", func() (*govcd.VApp, error) {
		endpoint, err := url.ParseRequestURI(vdc.Vdc.HREF)
		if err != nil {
			return nil, fmt.Errorf("invalid VDC HREF: %w", err)
		}
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/action/composeVApp"
		params := &types.ComposeVAppParams{
			Ovf: types.XMLNamespaceOVF, Xsi: types.XMLNamespaceXSI,
			Xmlns: types.XMLNamespaceVCloud, Name: name, Description: description,
			Deploy: false, PowerOn: false,
		}
		vapp := govcd.NewVApp(&client.Client)
		_, err = client.Client.ExecuteRequest(endpoint.String(),
			http.MethodPost, types.MimeComposeVappParams, "error creating vApp: %s", params, vapp.VApp)
		return vapp, err
	})
	if createErr != nil {
		if isPermanentError(createErr) && !strings.Contains(strings.ToLower(createErr.Error()), "already exists") {
			return nil, createErr
		}
		// The POST may have committed despite the error. Only retry discovery;
		// retain the generated name so the caller can clean up if recovery fails.
		var lookupErr error
		vapp, lookupErr = safeVCDCall(ctx, g, "GetVAppByName(recover create)", func() (*govcd.VApp, error) {
			return vdc.GetVAppByName(name, true)
		})
		if lookupErr != nil {
			return nil, fmt.Errorf("create vApp %q: %w; recovery lookup failed: %v", name, createErr, lookupErr)
		}
		g.log.Info("recovered vApp after ambiguous create", "name", name, "href", vapp.VApp.HREF)
	}
	if vapp.VApp.Tasks != nil {
		for _, taskInfo := range vapp.VApp.Tasks.Task {
			if taskInfo == nil {
				continue
			}
			task := govcd.NewTask(&client.Client)
			task.Task = taskInfo
			if err := g.waitTaskCompletion(ctx, task); err != nil {
				return nil, err
			}
		}
	}
	if err := safeVCDCallVoid(ctx, g, "Refresh vApp(create)", vapp.Refresh, preserveVApp(vapp)); err != nil {
		return nil, err
	}
	if vapp.VApp.Children != nil && len(vapp.VApp.Children.VM) != 0 {
		return nil, fmt.Errorf("recovered vApp %q is not empty", name)
	}
	return vapp, nil
}

// Release the SDK read lock between polls, allowing proactive renewal while a
// task runs. Bound the wait even if vCD never transitions the task out of running.
func (g *InstanceGroup) waitTaskCompletion(ctx context.Context, task *govcd.Task) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	for {
		if err := safeVCDCallVoid(ctx, g, "Refresh task", task.Refresh); err != nil {
			return err
		}
		switch task.Task.Status {
		case "success":
			return nil
		case "queued", "preRunning", "running":
		default:
			return fmt.Errorf("task %s ended with status %s: %+v", task.Task.HREF, task.Task.Status, task.Task.Error)
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
