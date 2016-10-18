package app

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"

	"github.com/gorilla/mux"
	"golang.org/x/net/context"

	"github.com/weaveworks/scope/probe/kubernetes"
	"github.com/weaveworks/scope/render"
	"github.com/weaveworks/scope/report"
)

const apiTopologyURL = "/api/topology/"
const processesTopologyDescID = "processes"
const processesByNameTopologyDescID = "processes-by-name"
const containersTopologyDescID = "containers"
const containersByHostnameTopologyDescID = "containers-by-hostname"
const containersByImageTopologyDescID = "containers-by-image"
const podsTopologyDescID = "pods"
const replicaSetsTopologyDescID = "replica-sets"
const deploymentsTopologyDescID = "deployments"
const servicesTopologyDescID = "services"
const hostsTopologyDescID = "hosts"

var (
	topologyRegistry = &registry{
		items: map[string]APITopologyDesc{},
	}
	k8sPseudoFilter = APITopologyOptionGroup{
		ID:      "pseudo",
		Default: "hide",
		Options: []APITopologyOption{
			{Value: "show", Label: "Show Unmanaged", filter: nil, filterPseudo: false},
			{Value: "hide", Label: "Hide Unmanaged", filter: render.IsNotPseudo, filterPseudo: true},
		},
	}
)

var containerFilters []APITopologyOptionGroup
var unconnectedFilter []APITopologyOptionGroup

// InitializeTopologies the topologies
func init() {
	// containerOpts holds the container topology view options such as "all" and "system"
	var containerOpts []APITopologyOption
	// Place at the beginning of the containerOpts,
	// the filter options that won't be changing
	containerOpts = append(containerOpts, APITopologyOption{Value: "all", Label: "All", filter: nil, filterPseudo: false})
	containerOpts = append(containerOpts, APITopologyOption{Value: "system", Label: "System Containers", filter: render.IsSystem, filterPseudo: false})
	containerOpts = append(containerOpts, APITopologyOption{Value: "notsystem", Label: "Application Containers", filter: render.IsApplication, filterPseudo: false})

	containerFilters = []APITopologyOptionGroup{
		{
			ID:      "system",
			Default: "application",
			Options: containerOpts,
		},
		{
			ID:      "stopped",
			Default: "running",
			Options: []APITopologyOption{
				{Value: "stopped", Label: "Stopped containers", filter: render.IsStopped, filterPseudo: false},
				{Value: "running", Label: "Running containers", filter: render.IsRunning, filterPseudo: false},
				{Value: "both", Label: "Both", filter: nil, filterPseudo: false},
			},
		},
		{
			ID:      "pseudo",
			Default: "hide",
			Options: []APITopologyOption{
				{Value: "show", Label: "Show Uncontained", filter: nil, filterPseudo: false},
				{Value: "hide", Label: "Hide Uncontained", filter: render.IsNotPseudo, filterPseudo: true},
			},
		},
	}

	unconnectedFilter = []APITopologyOptionGroup{
		{
			ID:      "unconnected",
			Default: "hide",
			Options: []APITopologyOption{
				// Show the user why there are filtered nodes in this view.
				// Don't give them the option to show those nodes.
				{Value: "hide", Label: "Unconnected nodes hidden", filter: nil, filterPseudo: false},
			},
		},
	}

	// Topology option labels should tell the current state. The first item must
	// be the verb to get to that state
	topologyRegistry.add(
		APITopologyDesc{
			id:          processesTopologyDescID,
			renderer:    render.FilterUnconnected(render.ProcessWithContainerNameRenderer),
			Name:        "Processes",
			Rank:        1,
			Options:     unconnectedFilter,
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:          processesByNameTopologyDescID,
			parent:      "processes",
			renderer:    render.FilterUnconnected(render.ProcessNameRenderer),
			Name:        "by name",
			Options:     unconnectedFilter,
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:       containersTopologyDescID,
			renderer: render.ContainerWithImageNameRenderer,
			Name:     "Containers",
			Rank:     2,
			Options:  containerFilters,
		},
		APITopologyDesc{
			id:       containersByHostnameTopologyDescID,
			parent:   "containers",
			renderer: render.ContainerHostnameRenderer,
			Name:     "by DNS name",
			Options:  containerFilters,
		},
		APITopologyDesc{
			id:       containersByImageTopologyDescID,
			parent:   "containers",
			renderer: render.ContainerImageRenderer,
			Name:     "by image",
			Options:  containerFilters,
		},
		APITopologyDesc{
			id:          podsTopologyDescID,
			renderer:    render.PodRenderer,
			Name:        "Pods",
			Rank:        3,
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:          replicaSetsTopologyDescID,
			parent:      "pods",
			renderer:    render.ReplicaSetRenderer,
			Name:        "replica sets",
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:          deploymentsTopologyDescID,
			parent:      "pods",
			renderer:    render.DeploymentRenderer,
			Name:        "deployments",
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:          servicesTopologyDescID,
			parent:      "pods",
			renderer:    render.PodServiceRenderer,
			Name:        "services",
			HideIfEmpty: true,
		},
		APITopologyDesc{
			id:       hostsTopologyDescID,
			renderer: render.HostRenderer,
			Name:     "Hosts",
			Rank:     4,
		},
	)
}

// MakeFilterOption provides an external interface to the package for creating an APITopologyOption
func MakeFilterOption(value string, label string, filterFunc render.FilterFunc) APITopologyOption {
	return APITopologyOption{Value: value, Label: label, filter: filterFunc, filterPseudo: false}
}

// kubernetesFilters generates the current kubernetes filters based on the
// available k8s topologies.
func kubernetesFilters(namespaces ...string) APITopologyOptionGroup {
	options := APITopologyOptionGroup{ID: "namespace", Default: "all"}
	for _, namespace := range namespaces {
		if namespace == "default" {
			options.Default = namespace
		}
		options.Options = append(options.Options, APITopologyOption{
			Value: namespace, Label: namespace, filter: render.IsNamespace(namespace), filterPseudo: false,
		})
	}
	options.Options = append(options.Options, APITopologyOption{Value: "all", Label: "All Namespaces", filter: nil, filterPseudo: false})
	return options
}

// updateFilters updates the available filters based on the current report.
// Currently only kubernetes changes.
func updateFilters(rpt report.Report, topologies []APITopologyDesc) []APITopologyDesc {
	namespaces := map[string]struct{}{}
	for _, t := range []report.Topology{rpt.Pod, rpt.Service, rpt.Deployment, rpt.ReplicaSet} {
		for _, n := range t.Nodes {
			if state, ok := n.Latest.Lookup(kubernetes.State); ok && state == kubernetes.StateDeleted {
				continue
			}
			if namespace, ok := n.Latest.Lookup(kubernetes.Namespace); ok {
				namespaces[namespace] = struct{}{}
			}
		}
	}
	var ns []string
	for namespace := range namespaces {
		ns = append(ns, namespace)
	}
	sort.Strings(ns)
	for i, t := range topologies {
		if t.id == podsTopologyDescID || t.id == servicesTopologyDescID || t.id == deploymentsTopologyDescID || t.id == replicaSetsTopologyDescID {
			topologies[i] = updateTopologyFilters(t, []APITopologyOptionGroup{
				kubernetesFilters(ns...), k8sPseudoFilter,
			})
		}
	}
	return topologies
}

// updateTopologyFilters recursively sets the options on a topology description
func updateTopologyFilters(t APITopologyDesc, options []APITopologyOptionGroup) APITopologyDesc {
	t.Options = options
	for i, sub := range t.SubTopologies {
		t.SubTopologies[i] = updateTopologyFilters(sub, options)
	}
	return t
}

// registry is a threadsafe store of the available topologies
type registry struct {
	sync.RWMutex
	items map[string]APITopologyDesc
}

// APITopologyDesc is returned in a list by the /api/topology handler.
type APITopologyDesc struct {
	id       string
	parent   string
	renderer render.Renderer

	Name        string                   `json:"name"`
	Rank        int                      `json:"rank"`
	HideIfEmpty bool                     `json:"hide_if_empty"`
	Options     []APITopologyOptionGroup `json:"options"`

	URL           string            `json:"url"`
	SubTopologies []APITopologyDesc `json:"sub_topologies,omitempty"`
	Stats         topologyStats     `json:"stats,omitempty"`
}

type byName []APITopologyDesc

func (a byName) Len() int           { return len(a) }
func (a byName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byName) Less(i, j int) bool { return a[i].Name < a[j].Name }

// APITopologyOptionGroup describes a group of APITopologyOptions
type APITopologyOptionGroup struct {
	ID      string              `json:"id"`
	Default string              `json:"defaultValue,omitempty"`
	Options []APITopologyOption `json:"options,omitempty"`
}

// APITopologyOption describes a &param=value to a given topology.
type APITopologyOption struct {
	Value string `json:"value"`
	Label string `json:"label"`

	filter       render.FilterFunc
	filterPseudo bool
}

type topologyStats struct {
	NodeCount          int `json:"node_count"`
	NonpseudoNodeCount int `json:"nonpseudo_node_count"`
	EdgeCount          int `json:"edge_count"`
	FilteredNodes      int `json:"filtered_nodes"`
}

// AddContainerFilters adds to the topology registry's containerFilters
func AddContainerFilters(newOptions ...APITopologyOption) {
	topologyRegistry.addContainerFilters(newOptions...)
}

func (r *registry) addContainerFilters(newOptions ...APITopologyOption) {
	r.Lock()
	defer r.Unlock()

	for _, key := range []string{containersTopologyDescID, containersByHostnameTopologyDescID, containersByImageTopologyDescID} {
		var tmp = r.items[key]
		//for _, optionGroup := range tmp.Options {
		for i := 0; i < len(tmp.Options); i++ {
			if tmp.Options[i].ID == "system" {
				tmp.Options[i].Options = append(tmp.Options[i].Options, newOptions...)
				r.items[key] = tmp
			}
		}
	}
}

func (r *registry) add(ts ...APITopologyDesc) {
	r.Lock()
	defer r.Unlock()
	for _, t := range ts {
		t.URL = apiTopologyURL + t.id

		if t.parent != "" {
			parent := r.items[t.parent]
			parent.SubTopologies = append(parent.SubTopologies, t)
			r.items[t.parent] = parent
		}

		r.items[t.id] = t
	}
}

func (r *registry) get(name string) (APITopologyDesc, bool) {
	r.RLock()
	defer r.RUnlock()
	t, ok := r.items[name]
	return t, ok
}

func (r *registry) walk(f func(APITopologyDesc)) {
	r.RLock()
	defer r.RUnlock()
	descs := []APITopologyDesc{}
	for _, desc := range r.items {
		if desc.parent != "" {
			continue
		}
		descs = append(descs, desc)
	}
	sort.Sort(byName(descs))
	for _, desc := range descs {
		f(desc)
	}
}

// makeTopologyList returns a handler that yields an APITopologyList.
func (r *registry) makeTopologyList(rep Reporter) CtxHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, req *http.Request) {
		report, err := rep.Report(ctx)
		if err != nil {
			respondWith(w, http.StatusInternalServerError, err)
			return
		}
		respondWith(w, http.StatusOK, r.renderTopologies(report, req))
	}
}

func (r *registry) renderTopologies(rpt report.Report, req *http.Request) []APITopologyDesc {
	topologies := []APITopologyDesc{}
	req.ParseForm()
	r.walk(func(desc APITopologyDesc) {
		renderer, decorator, _ := r.rendererForTopology(desc.id, req.Form, rpt)
		desc.Stats = decorateWithStats(rpt, renderer, decorator)
		for i, sub := range desc.SubTopologies {
			renderer, decorator, _ := r.rendererForTopology(sub.id, req.Form, rpt)
			desc.SubTopologies[i].Stats = decorateWithStats(rpt, renderer, decorator)
		}
		topologies = append(topologies, desc)
	})
	return updateFilters(rpt, topologies)
}

func decorateWithStats(rpt report.Report, renderer render.Renderer, decorator render.Decorator) topologyStats {
	var (
		nodes     int
		realNodes int
		edges     int
	)
	for _, n := range renderer.Render(rpt, decorator) {
		nodes++
		if n.Topology != render.Pseudo {
			realNodes++
		}
		edges += len(n.Adjacency)
	}
	renderStats := renderer.Stats(rpt, decorator)
	return topologyStats{
		NodeCount:          nodes,
		NonpseudoNodeCount: realNodes,
		EdgeCount:          edges,
		FilteredNodes:      renderStats.FilteredNodes,
	}
}

func (r *registry) rendererForTopology(topologyID string, values url.Values, rpt report.Report) (render.Renderer, render.Decorator, error) {
	topology, ok := r.get(topologyID)
	if !ok {
		return nil, nil, fmt.Errorf("topology not found: %s", topologyID)
	}
	topology = updateFilters(rpt, []APITopologyDesc{topology})[0]

	var decorators []render.Decorator
	for _, group := range topology.Options {
		value := values.Get(group.ID)
		for _, opt := range group.Options {
			if opt.filter == nil {
				continue
			}
			if (value == "" && group.Default == opt.Value) || (opt.Value != "" && opt.Value == value) {
				if opt.filterPseudo {
					decorators = append(decorators, render.MakeFilterPseudoDecorator(opt.filter))
				} else {
					decorators = append(decorators, render.MakeFilterDecorator(opt.filter))
				}
			}
		}
	}
	if len(decorators) > 0 {
		return topology.renderer, render.ComposeDecorators(decorators...), nil
	}
	return topology.renderer, nil, nil
}

type reporterHandler func(context.Context, Reporter, http.ResponseWriter, *http.Request)

func captureReporter(rep Reporter, f reporterHandler) CtxHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		f(ctx, rep, w, r)
	}
}

type rendererHandler func(context.Context, render.Renderer, render.Decorator, report.Report, http.ResponseWriter, *http.Request)

func (r *registry) captureRenderer(rep Reporter, f rendererHandler) CtxHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, req *http.Request) {
		topologyID := mux.Vars(req)["topology"]
		if _, ok := r.get(topologyID); !ok {
			http.NotFound(w, req)
			return
		}
		rpt, err := rep.Report(ctx)
		if err != nil {
			respondWith(w, http.StatusInternalServerError, err)
			return
		}
		req.ParseForm()
		renderer, decorator, err := r.rendererForTopology(topologyID, req.Form, rpt)
		if err != nil {
			respondWith(w, http.StatusInternalServerError, err)
			return
		}
		f(ctx, renderer, decorator, rpt, w, req)
	}
}
