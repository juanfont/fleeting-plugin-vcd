package vcd

import (
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/mux"
	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type DebugServer struct {
	log               hclog.Logger
	store             *desiredStateStore
	router            *mux.Router
	instanceGroupName string
}

func NewDebugServer(log hclog.Logger, store *desiredStateStore, instanceGroupName string) *DebugServer {
	ds := &DebugServer{
		log:               log,
		store:             store,
		router:            mux.NewRouter(),
		instanceGroupName: instanceGroupName,
	}

	ds.setupRoutes()
	return ds
}

func (ds *DebugServer) setupRoutes() {
	ds.router.HandleFunc("/", ds.handleInstancesTable).Methods("GET")
	ds.router.HandleFunc("/instances", ds.handleInstancesTable).Methods("GET")
	ds.router.Handle("/metrics", promhttp.Handler()).Methods("GET")
}

func (ds *DebugServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ds.router.ServeHTTP(w, r)
}

func (ds *DebugServer) handleInstancesTable(w http.ResponseWriter, r *http.Request) {
	ds.log.Debug("Serving instances debug table")

	instances := ds.store.GetAll()

	// Sort by CreatedAt from newest to oldest
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].CreatedAt == nil && instances[j].CreatedAt == nil {
			return false
		}
		if instances[i].CreatedAt == nil {
			return false
		}
		if instances[j].CreatedAt == nil {
			return true
		}
		return instances[i].CreatedAt.After(*instances[j].CreatedAt)
	})

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
        .header { margin-bottom: 20px; }
        .refresh-info { margin-top: 20px; font-size: 0.9em; color: #666; }
        .deleted-row { background-color: #f8f9fa; opacity: 0.6; color: #6c757d; }
        .deleting-row { background-color: #f8d7da; }
        .creating-row { background-color: #fff3cd; }
        .preexisting-row { background-color: #fff3e0; }
        .preexisting-yes { background-color: #ff9800; color: #ffffff; font-weight: bold; text-align: center; }
        .preexisting-no { color: #28a745; font-weight: bold; text-align: center; }
        .phase { font-weight: bold; text-align: center; padding: 4px 8px; border-radius: 4px; }
        .phase-PendingCreate { background-color: #fff3cd; color: #856404; }
        .phase-Creating { background-color: #cce5ff; color: #004085; }
        .phase-Running { background-color: #d4edda; color: #155724; }
        .phase-PendingDelete { background-color: #f8d7da; color: #721c24; }
        .phase-Deleting { background-color: #f5c6cb; color: #721c24; }
        .phase-Deleted { background-color: #f8f9fa; color: #6c757d; }
        .retry-count { font-weight: bold; color: #dc3545; }
        .error-text { color: #dc3545; font-size: 0.85em; max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
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
                <th>Intent ID</th>
                <th>VApp HREF</th>
                <th>Preexisting</th>
                <th>Phase</th>
                <th>VApp Name</th>
                <th>VM Name</th>
                <th>VApp Status</th>
                <th>VM Status</th>
                <th>IP Address</th>
                <th>Retries</th>
                <th>Last Error</th>
                <th>Next Retry</th>
                <th>Created At</th>
                <th>Create Started</th>
                <th>Create Completed</th>
                <th>Delete Requested</th>
                <th>Delete Started</th>
                <th>Delete Completed</th>
                <th>GC Marked</th>
                <th>Last Updated</th>
            </tr>
        </thead>
        <tbody>
            {{range .Instances}}
            <tr class="{{if eq .Phase.String "Deleted"}}deleted-row{{else if eq .Phase.String "Deleting"}}deleting-row{{else if eq .Phase.String "PendingDelete"}}deleting-row{{else if eq .Phase.String "Creating"}}creating-row{{else if eq .Phase.String "PendingCreate"}}creating-row{{else if not .CreatedAt}}preexisting-row{{end}}">
                <td>{{.IntentID}}</td>
                <td>{{.ID}}</td>
                <td class="{{if not .CreatedAt}}preexisting-yes{{else}}preexisting-no{{end}}">{{if not .CreatedAt}}YES{{else}}NO{{end}}</td>
                <td class="phase phase-{{.Phase.String}}">{{.Phase.String}}</td>
                <td>{{.Name}}</td>
                <td>{{.VMName}}</td>
                <td>{{.VAppStatus}}</td>
                <td>{{.VMStatus}}</td>
                <td>{{.IPAddress}}</td>
                <td class="{{if gt .RetryCount 0}}retry-count{{end}}">{{.RetryCount}}</td>
                <td class="error-text" title="{{.LastError}}">{{.LastError}}</td>
                <td>{{if .NextRetryAfter}}{{.NextRetryAfter.Format "15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .CreatedAt}}{{.CreatedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .CreateStartedAt}}{{.CreateStartedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .CreateCompletedAt}}{{.CreateCompletedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .DeleteRequestedAt}}{{.DeleteRequestedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .DeleteStartedAt}}{{.DeleteStartedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .DeleteCompletedAt}}{{.DeleteCompletedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .GCMarkedAt}}{{.GCMarkedAt.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
                <td>{{if .LastUpdated}}{{.LastUpdated.Format "2006-01-02 15:04:05"}}{{else}}-{{end}}</td>
            </tr>
            {{else}}
            <tr>
                <td colspan="20" style="text-align: center; font-style: italic;">No instances found</td>
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
		Instances                []Instance
		InstanceGroupName        string
		InstanceGroupMetadataKey string
		LastUpdated              string
	}{
		Instances:                instances,
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
