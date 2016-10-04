package app

import (
	"github.com/weaveworks/scope/render"
	"math/rand"
	"strings"
	"time"
)

type arrayFlags []string

func (i *arrayFlags) String() string {
	return "my string representation"
}
func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

// ContainerLabelFlags is set from the command line argument app.container-label-filter in /scope/prog/main.go
var ContainerLabelFlags arrayFlags

type filter struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Label string `json:"label"`
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randStringBytes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func getContainerTopologyOptions() ([]APITopologyOption, error) {
	var toptions []APITopologyOption

	// Add option to view weave system containers
	system := APITopologyOption{Value: "system", Label: "System Containers", filter: render.IsSystem, filterPseudo: false}
	toptions = append(toptions, system)

	// Add option to not view weave system containers
	notSystem := APITopologyOption{Value: "notsystem", Label: "Application Containers", filter: render.IsApplication, filterPseudo: false}
	toptions = append(toptions, notSystem)

	for _, f := range ContainerLabelFlags {
		titleLabel := strings.Split(f, ":")

		v := APITopologyOption{Value: randStringBytes(10), Label: titleLabel[0], filter: render.IsDesired(titleLabel[1]), filterPseudo: false}
		toptions = append(toptions, v)
	}

	// Add option to view all
	all := APITopologyOption{Value: "all", Label: "All", filter: nil, filterPseudo: false}
	toptions = append(toptions, all)

	return toptions, nil
}
