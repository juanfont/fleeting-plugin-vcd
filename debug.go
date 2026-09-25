package vcd

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed web
var webFS embed.FS

// phaseOrder lists phases in the order the debug page shows them.
var phaseOrder = []Phase{PhaseCreating, PhasePendingCreate, PhaseRunning, PhaseDeleting, PhasePendingDelete, PhaseDeleted}

type DebugServer struct {
	log               hclog.Logger
	store             *desiredStateStore
	status            func() DebugStatus
	router            *mux.Router
	instanceGroupName string
	tmpl              *template.Template
}

// NewDebugServer serves a live view of the instance group. status may be nil.
func NewDebugServer(log hclog.Logger, store *desiredStateStore, instanceGroupName string, status func() DebugStatus) *DebugServer {
	if status == nil {
		status = func() DebugStatus { return DebugStatus{} }
	}
	ds := &DebugServer{
		log:               log,
		store:             store,
		status:            status,
		router:            mux.NewRouter(),
		instanceGroupName: instanceGroupName,
		tmpl:              template.Must(template.New("").Funcs(templateFuncs).ParseFS(webFS, "web/templates/*.html")),
	}
	ds.setupRoutes()
	return ds
}

func (ds *DebugServer) setupRoutes() {
	static, _ := fs.Sub(webFS, "web/static")
	ds.router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.FS(static)))).Methods("GET")
	ds.router.HandleFunc("/", ds.handlePage).Methods("GET")
	ds.router.HandleFunc("/instances", ds.handlePage).Methods("GET")
	ds.router.HandleFunc("/fragments/summary", ds.handleSummary).Methods("GET")
	ds.router.HandleFunc("/fragments/instances", ds.handleInstances).Methods("GET")
	ds.router.HandleFunc("/api/status", ds.handleAPIStatus).Methods("GET")
	ds.router.Handle("/metrics", promhttp.Handler()).Methods("GET")
}

func (ds *DebugServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ds.router.ServeHTTP(w, r)
}

type instanceView struct {
	Instance
	Source     string // created, leftover, preexisting
	PhaseSince *time.Time
}

type summaryView struct {
	InstanceGroupName string
	Status            DebugStatus
	Phases            []phaseCount
	Total             int
	Now               time.Time
}

type phaseCount struct {
	Phase string
	Count int
}

func (ds *DebugServer) summary() summaryView {
	counts := map[Phase]int{}
	all := ds.store.GetAll()
	for _, inst := range all {
		counts[inst.Phase]++
	}
	v := summaryView{InstanceGroupName: ds.instanceGroupName, Status: ds.status(), Total: len(all), Now: time.Now()}
	for _, p := range phaseOrder {
		if counts[p] > 0 {
			v.Phases = append(v.Phases, phaseCount{p.String(), counts[p]})
		}
	}
	return v
}

func (ds *DebugServer) instances(hideDeleted bool, phase string) []instanceView {
	var out []instanceView
	for _, inst := range ds.store.GetAll() {
		if hideDeleted && inst.Phase == PhaseDeleted {
			continue
		}
		if phase != "" && inst.Phase.String() != phase {
			continue
		}
		out = append(out, instanceView{Instance: inst, Source: instanceSource(inst), PhaseSince: phaseSince(inst)})
	}
	rank := map[Phase]int{}
	for i, p := range phaseOrder {
		rank[p] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Phase] != rank[out[j].Phase] {
			return rank[out[i].Phase] < rank[out[j].Phase]
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		// Stable order for unnamed intents, so rows do not shuffle between refreshes.
		return out[i].IntentID < out[j].IntentID
	})
	return out
}

func instanceSource(inst Instance) string {
	switch {
	case inst.Leftover:
		return "leftover"
	case inst.CreatedAt == nil:
		return "preexisting"
	default:
		return "created"
	}
}

// phaseSince returns when the instance entered its current phase, as far as
// the recorded timestamps tell.
func phaseSince(inst Instance) *time.Time {
	switch inst.Phase {
	case PhasePendingCreate:
		return inst.CreatedAt
	case PhaseCreating:
		return inst.CreateStartedAt
	case PhaseRunning:
		return inst.CreateCompletedAt
	case PhasePendingDelete:
		return inst.DeleteRequestedAt
	case PhaseDeleting:
		return inst.DeleteStartedAt
	case PhaseDeleted:
		return inst.DeleteCompletedAt
	}
	return nil
}

func (ds *DebugServer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ds.tmpl.ExecuteTemplate(w, name, data); err != nil {
		ds.log.Error("debug page render failed", "template", name, "error", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func (ds *DebugServer) handlePage(w http.ResponseWriter, r *http.Request) {
	ds.render(w, "page.html", struct {
		Summary   summaryView
		Instances []instanceView
		Phases    []string
	}{ds.summary(), ds.instances(true, ""), phaseNames()})
}

func (ds *DebugServer) handleSummary(w http.ResponseWriter, r *http.Request) {
	ds.render(w, "summary.html", ds.summary())
}

func (ds *DebugServer) handleInstances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ds.render(w, "instances.html", ds.instances(q.Get("hide_deleted") != "", q.Get("phase")))
}

type apiInstance struct {
	IntentID  string     `json:"intent_id"`
	HREF      string     `json:"href,omitempty"`
	Name      string     `json:"name,omitempty"`
	Phase     string     `json:"phase"`
	Source    string     `json:"source"`
	Leftover  bool       `json:"leftover"`
	IPAddress string     `json:"ip,omitempty"`
	Retries   int        `json:"retries"`
	LastError string     `json:"last_error,omitempty"`
	Since     *time.Time `json:"phase_since,omitempty"`
}

func (ds *DebugServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	sum := ds.summary()
	phases := map[string]int{}
	for _, p := range sum.Phases {
		phases[p.Phase] = p.Count
	}
	var insts []apiInstance
	for _, v := range ds.instances(false, "") {
		insts = append(insts, apiInstance{
			IntentID: v.IntentID, HREF: v.ID, Name: v.Name, Phase: v.Phase.String(), Source: v.Source,
			Leftover: v.Leftover, IPAddress: v.IPAddress, Retries: v.RetryCount, LastError: v.LastError, Since: v.PhaseSince,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"instance_group": ds.instanceGroupName,
		"status":         sum.Status,
		"phases":         phases,
		"instances":      insts,
	}); err != nil {
		ds.log.Error("debug status encode failed", "error", err)
	}
}

func phaseNames() []string {
	var out []string
	for _, p := range phaseOrder {
		out = append(out, p.String())
	}
	return out
}

var templateFuncs = template.FuncMap{
	// ago renders a timestamp relative to now, e.g. "3m12s ago".
	"ago": func(t any) string {
		var ts time.Time
		switch v := t.(type) {
		case time.Time:
			ts = v
		case *time.Time:
			if v == nil {
				return "-"
			}
			ts = *v
		}
		if ts.IsZero() {
			return "-"
		}
		d := time.Since(ts)
		if d < 0 {
			return "in " + (-d).Round(time.Second).String()
		}
		return d.Round(time.Second).String() + " ago"
	},
	"until": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return time.Until(t).Round(time.Second).String()
	},
	"clock": func(t any) string {
		switch v := t.(type) {
		case time.Time:
			if v.IsZero() {
				return ""
			}
			return v.UTC().Format(time.RFC3339)
		case *time.Time:
			if v == nil {
				return ""
			}
			return v.UTC().Format(time.RFC3339)
		}
		return ""
	},
	"releaseIn": func(st DebugStatus) string {
		if st.HoldStartedAt.IsZero() {
			return "after the first complete poll"
		}
		left := time.Duration(st.HoldTimeoutSeconds*float64(time.Second)) - time.Since(st.HoldStartedAt)
		if left < 0 {
			left = 0
		}
		return fmt.Sprintf("in %s at the latest", left.Round(time.Second))
	},
	// shortHREF shows the vApp id's first block, e.g. "vapp-042dfab4".
	"shortHREF": func(href string) string {
		if i := strings.LastIndex(href, "/"); i >= 0 {
			href = href[i+1:]
		}
		if len(href) > 13 {
			return href[:13]
		}
		return href
	},
	"seconds": func(s float64) string {
		return (time.Duration(s * float64(time.Second))).Round(time.Millisecond).String()
	},
}
