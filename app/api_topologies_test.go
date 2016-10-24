package app_test

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"
	"fmt"

	"github.com/gorilla/mux"
	"github.com/ugorji/go/codec"
	"k8s.io/kubernetes/pkg/api"

	"github.com/weaveworks/scope/app"
	"github.com/weaveworks/scope/render"
	"github.com/weaveworks/scope/probe/kubernetes"
	"github.com/weaveworks/scope/report"
	"github.com/weaveworks/scope/test/fixture"
)

func TestAPITopology(t *testing.T) {
	ts := topologyServer()
	defer ts.Close()

	body := getRawJSON(t, ts, "/api/topology")

	var topologies []app.APITopologyDesc
	decoder := codec.NewDecoderBytes(body, &codec.JsonHandle{})
	if err := decoder.Decode(&topologies); err != nil {
		t.Fatalf("JSON parse error: %s", err)
	}
	equals(t, 4, len(topologies))

	for _, topology := range topologies {
		is200(t, ts, topology.URL)

		for _, subTopology := range topology.SubTopologies {
			is200(t, ts, subTopology.URL)
		}

		if have := topology.Stats.EdgeCount; have <= 0 {
			t.Errorf("EdgeCount isn't positive for %s: %d", topology.Name, have)
		}

		if have := topology.Stats.NodeCount; have <= 0 {
			t.Errorf("NodeCount isn't positive for %s: %d", topology.Name, have)
		}

		if have := topology.Stats.NonpseudoNodeCount; have <= 0 {
			t.Errorf("NonpseudoNodeCount isn't positive for %s: %d", topology.Name, have)
		}
	}
}

func TestAPITopologyAddsContainerLabelFilters(t *testing.T) {
	ts := topologyServer()
	defer ts.Close()

	body := getRawJSON(t, ts, "/api/topology")

	var topologies []app.APITopologyDesc
	decoder := codec.NewDecoderBytes(body, &codec.JsonHandle{})
	if err := decoder.Decode(&topologies); err != nil {
		t.Fatalf("JSON parse error: %s", err)
	}
	equals(t, 4, len(topologies))

	topologyRegistry := &app.Registry{
		Items: map[string]app.APITopologyDesc{},
	}

	for _, topology := range topologies {
		topologyRegistry.Add(topology)
	}

	var containerOpts []app.APITopologyOption
	containerOpts = append(containerOpts, app.MakeFilterOption("all", "All", nil, false))
	containerOpts = append(containerOpts, app.MakeFilterOption("system", "System Containers", render.IsSystem, false))
	containerOpts = append(containerOpts, app.MakeFilterOption("notsystem", "Application Containers", render.IsApplication, false))
	containerFilters := []app.APITopologyOptionGroup{
		app.MakeAPITopologyOptionGroup( "system", "application", containerOpts,
		),
		app.MakeAPITopologyOptionGroup( "stopped", "running",
			[]app.APITopologyOption{
				app.MakeFilterOption("stopped", "Stopped containers", render.IsStopped, false),
				app.MakeFilterOption("running", "Running containers", render.IsRunning, false),
				app.MakeFilterOption("both", "Both", nil, false),
			},
		),
		app.MakeAPITopologyOptionGroup( "pseudo", "hide",
			[]app.APITopologyOption{
				app.MakeFilterOption("show", "Show Uncontained", nil, false),
				app.MakeFilterOption("hide", "Hide Uncontained", render.IsNotPseudo, true),
			},
		),
	}

	unconnectedFilter := []app.APITopologyOptionGroup{
		app.MakeAPITopologyOptionGroup( "unconnected", "hide",
			[]app.APITopologyOption{
				// Show the user why there are filtered nodes in this view.
				// Don't give them the option to show those nodes.
				app.MakeFilterOption("hide", "Unconnected nodes hidden", nil, false),
			},
		),
	}

	topologyRegistry.Add(
		app.MakeAPITopologyDesc(
			"processes", 
			render.FilterUnconnected(render.ProcessWithContainerNameRenderer),
			"Processes",
			1,
			unconnectedFilter,
			true,
		),
		app.MakeAPITopologyDescWithParent(
			"processes-by-name",
			"processes",
			render.FilterUnconnected(render.ProcessNameRenderer),
			"by name",
			0,
			unconnectedFilter,
			true,
		),
		app.MakeAPITopologyDesc(
			"containers",
			render.ContainerWithImageNameRenderer,
			"Containers",
			2,
			containerFilters,
			false,
		),
	)

	equals(t, 3, topologyRegistry.CountContainerFilters())

	v := app.MakeFilterOption(fmt.Sprintf("cmdlinefilter%d", 0), "title", render.HasLabel("role=system"), false)
	topologyRegistry.AddContainerFilters(v)
	
	equals(t, 4, topologyRegistry.CountContainerFilters())

}

func TestAPITopologyAddsKubernetes(t *testing.T) {
	router := mux.NewRouter()
	c := app.NewCollector(1 * time.Minute)
	app.RegisterReportPostHandler(c, router)
	app.RegisterTopologyRoutes(router, c)
	ts := httptest.NewServer(router)
	defer ts.Close()

	body := getRawJSON(t, ts, "/api/topology")

	var topologies []app.APITopologyDesc
	decoder := codec.NewDecoderBytes(body, &codec.JsonHandle{})
	if err := decoder.Decode(&topologies); err != nil {
		t.Fatalf("JSON parse error: %s", err)
	}
	equals(t, 4, len(topologies))

	// Enable the kubernetes topologies
	rpt := report.MakeReport()
	rpt.Pod = report.MakeTopology()
	rpt.Pod.Nodes[fixture.ClientPodNodeID] = kubernetes.NewPod(&api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name:      "pong-a",
			Namespace: "ping",
			Labels:    map[string]string{"ponger": "true"},
		},
		Status: api.PodStatus{
			HostIP: "1.2.3.4",
			ContainerStatuses: []api.ContainerStatus{
				{ContainerID: "container1"},
				{ContainerID: "container2"},
			},
		},
	}).GetNode("")
	buf := &bytes.Buffer{}
	encoder := codec.NewEncoder(buf, &codec.MsgpackHandle{})
	if err := encoder.Encode(rpt); err != nil {
		t.Fatalf("GOB encoding error: %s", err)
	}
	checkRequest(t, ts, "POST", "/api/report", buf.Bytes())

	body = getRawJSON(t, ts, "/api/topology")
	decoder = codec.NewDecoderBytes(body, &codec.JsonHandle{})
	if err := decoder.Decode(&topologies); err != nil {
		t.Fatalf("JSON parse error: %s", err)
	}
	equals(t, 4, len(topologies))

	found := false
	for _, topology := range topologies {
		if topology.Name == "Pods" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Could not find pods topology")
	}
}
