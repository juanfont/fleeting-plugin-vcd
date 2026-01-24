package vcd

import (
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/mux"
	"github.com/hashicorp/go-hclog"
)

type DebugServer struct {
	log               hclog.Logger
	stateManager      *instanceStateManager
	router            *mux.Router
	instanceGroupName string
}

func NewDebugServer(log hclog.Logger, stateManager *instanceStateManager, instanceGroupName string) *DebugServer {
	ds := &DebugServer{
		log:               log,
		stateManager:      stateManager,
		router:            mux.NewRouter(),
		instanceGroupName: instanceGroupName,
	}

	ds.setupRoutes()
	return ds
}

func (ds *DebugServer) setupRoutes() {
	ds.router.HandleFunc("/", ds.handleInstancesTable).Methods("GET")
	ds.router.HandleFunc("/instances", ds.handleInstancesTable).Methods("GET")
}

func (ds *DebugServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ds.router.ServeHTTP(w, r)
}

func (ds *DebugServer) handleInstancesTable(w http.ResponseWriter, r *http.Request) {
	ds.log.Debug("Serving instances debug table")

	// Collect all instances from the state manager (including soft-deleted ones)
	instances := ds.stateManager.GetAll()

	// Enhance instances with fleeting state information
	type enhancedInstanceData struct {
		instanceData
		FleetingStateStr string
	}

	var enhancedInstances []enhancedInstanceData
	for _, instance := range instances {
		_, fleetingState := ds.stateManager.GetFleetingState(instance.InstanceID)
		enhancedInstances = append(enhancedInstances, enhancedInstanceData{
			instanceData:     instance,
			FleetingStateStr: string(fleetingState),
		})
	}

	// Sort by CreatedAt from newest to oldest
	sort.Slice(enhancedInstances, func(i, j int) bool {
		// Handle nil CreatedAt (preexisting instances) - put them at the end
		if enhancedInstances[i].CreatedAt == nil && enhancedInstances[j].CreatedAt == nil {
			return false // maintain original order for both nil
		}
		if enhancedInstances[i].CreatedAt == nil {
			return false // i goes after j (nil goes to end)
		}
		if enhancedInstances[j].CreatedAt == nil {
			return true // i goes before j (non-nil goes before nil)
		}
		// Both have CreatedAt, sort newest first
		return enhancedInstances[i].CreatedAt.After(*enhancedInstances[j].CreatedAt)
	})

	// HTML template for the table
	tmpl := `
<!DOCTYPE html>
<html>
<head>
    <title>VCD Fleeting Plugin - Instance State Debug</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; }
        table { border-collapse: collapse; width: 100%; }
        th, td { border: 1px solid #ddd; padding: 8px; text-align: left; }
        th { background-color: #f2f2f2; }
        .status-true { color: green; font-weight: bold; }
        .status-false { color: red; }
        .header { margin-bottom: 20px; }
        .refresh-info { margin-top: 20px; font-size: 0.9em; color: #666; }
        .deleted-row { background-color: #f8f9fa; opacity: 0.6; color: #6c757d; }
        .deleting-row { background-color: #f8d7da; }
        .preexisting-row { background-color: #fff3e0; }
        .preexisting-yes { background-color: #ff9800; color: #ffffff; font-weight: bold; text-align: center; }
        .preexisting-no { color: #28a745; font-weight: bold; text-align: center; }
        .fleeting-state { font-weight: bold; text-align: center; padding: 4px 8px; border-radius: 4px; }
        .state-creating { background-color: #fff3cd; color: #856404; }
        .state-running { background-color: #d4edda; color: #155724; }
        .state-deleting { background-color: #f8d7da; color: #721c24; }
        .state-deleted { background-color: #f8f9fa; color: #6c757d; }
        .state-timeout { background-color: #f5c6cb; color: #721c24; }
    </style>
    <meta http-equiv="refresh" content="5">
</head>
<body>
    <div class="header">
        <h1>VCD Fleeting Plugin - Instance State Debug</h1>
		<p>Instance group name: <b>{{.InstanceGroupName}}</b> (via vApp metadata key tag <b>{{.InstanceGroupMetadataKey}}</b>)</p>
        <p>Total Instances: <b>{{len .Instances}}</b></p>
        <p>Last Updated: <b>{{.LastUpdated}}</b></p>
    </div>
    
    <table>
        <thead>
            <tr>
                <th>Instance ID</th>
                <th>Preexisting</th>
                <th>Fleeting State</th>
                <th>VApp Name</th>
                <th>VM Name</th>
                <th>VApp Status</th>
                <th>VM Status</th>
                <th>IP Address</th>
                <th>Last Updated</th>
                <th>Created At</th>
                <th>Booting At</th>
                <th>Booted At</th>
                <th>Deleting At</th>
				<th>Garbage Collected At</th>
                <th>Deleted At</th>
            </tr>
        </thead>
        <tbody>
            {{range .Instances}}
            <tr class="{{if .DeletedAt}}deleted-row{{else if .DeletingAt}}deleting-row{{else if not .CreatedAt}}preexisting-row{{end}}">
                <td>{{.InstanceID}}</td>
                <td class="{{if not .CreatedAt}}preexisting-yes{{else}}preexisting-no{{end}}">{{if not .CreatedAt}}YES{{else}}NO{{end}}</td>
                <td class="fleeting-state state-{{.FleetingStateStr}}">{{.FleetingStateStr}}</td>
                <td>{{.VAppName}}</td>
                <td>{{.VMName}}</td>
                <td>{{.VAppStatus}}</td>
                <td>{{.VMStatus}}</td>
                <td>{{.IPAddress}}</td>
                <td>{{if .LastUpdated}}{{.LastUpdated.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .CreatedAt}}{{.CreatedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
				<td>{{if .BootingAt}}{{.BootingAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .BootedAt}}{{.BootedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .DeletingAt}}{{.DeletingAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
				<td>{{if .GarbageCollectedAt}}{{.GarbageCollectedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
				<td>{{if .DeletedAt}}{{.DeletedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
            </tr>
            {{else}}
            <tr>
                <td colspan="13" style="text-align: center; font-style: italic;">No instances found</td>
            </tr>
            {{end}}
        </tbody>
    </table>
    
    <div class="refresh-info">
        <p>This page auto-refreshes every 5 seconds.</p>
    </div>
</body>
</html>`

	t, err := template.New("instances").Parse(tmpl)
	if err != nil {
		ds.log.Error("Failed to parse template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Instances                []enhancedInstanceData
		InstanceGroupName        string
		InstanceGroupMetadataKey string
		LastUpdated              string
	}{
		Instances:                enhancedInstances,
		InstanceGroupName:        ds.instanceGroupName,
		InstanceGroupMetadataKey: instanceGroupMetadataKey,
		LastUpdated:              time.Now().Format("2006-01-02 15:04:05"),
	}

	w.Header().Set("Content-Type", "text/html")
	if err := t.Execute(w, data); err != nil {
		ds.log.Error("Failed to execute template", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}
