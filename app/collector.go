package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ugorji/go/codec"
	"golang.org/x/net/context"

	"github.com/weaveworks/scope/common/mtime"
	"github.com/weaveworks/scope/report"
)

// Reporter is something that can produce reports on demand. It's a convenient
// interface for parts of the app, and several experimental components.
type Reporter interface {
	Report(context.Context) (report.Report, error)
	WaitOn(context.Context, chan struct{})
	UnWait(context.Context, chan struct{})
}

// Adder is something that can accept reports. It's a convenient interface for
// parts of the app, and several experimental components.  It takes the following
// arguments:
// - context.Context: the request context
// - report.Report: the deserialised report
// - []byte: the serialised report (as gzip'd msgpack)
type Adder interface {
	Add(context.Context, report.Report, []byte) error
}

// A Collector is a Reporter and an Adder
type Collector interface {
	Reporter
	Adder
}

// Collector receives published reports from multiple producers. It yields a
// single merged report, representing all collected reports.
type collector struct {
	mtx        sync.Mutex
	reports    []report.Report
	timestamps []time.Time
	window     time.Duration
	cached     *report.Report
	merger     Merger
	waitableCondition
}

type waitableCondition struct {
	sync.Mutex
	waiters map[chan struct{}]struct{}
}

func (wc *waitableCondition) WaitOn(_ context.Context, waiter chan struct{}) {
	wc.Lock()
	wc.waiters[waiter] = struct{}{}
	wc.Unlock()
}

func (wc *waitableCondition) UnWait(_ context.Context, waiter chan struct{}) {
	wc.Lock()
	delete(wc.waiters, waiter)
	wc.Unlock()
}

func (wc *waitableCondition) Broadcast() {
	wc.Lock()
	for waiter := range wc.waiters {
		// Non-block write to channel
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
	wc.Unlock()
}

// NewCollector returns a collector ready for use.
func NewCollector(window time.Duration) Collector {
	return &collector{
		window: window,
		waitableCondition: waitableCondition{
			waiters: map[chan struct{}]struct{}{},
		},
		merger: NewSmartMerger(),
	}
}

// Add adds a report to the collector's internal state. It implements Adder.
func (c *collector) Add(_ context.Context, rpt report.Report, _ []byte) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	fmt.Println("Add()")

	var containersToRemove []string 

	fmt.Println("container:")
	fmt.Println(rpt.Container.Shape)//rpt.Container)
	for k, v := range rpt.Container.Nodes {
    	fmt.Println(k)
		if val, ok := v.Latest.Lookup("docker_env_A8_SERVICE"); ok {
			fmt.Println("found it!")
			fmt.Println(val)
			// TODO - delete if value is equal to current space ID as found from the environment variable

			// for now... delete all edges
			v.Adjacency = report.EmptyIDList
			v.Edges = report.EmptyEdgeMetadatas
		} else {
			containersToRemove = append(containersToRemove, k)
			delete(rpt.Container.Nodes, k)
			continue
		}
	}

	// Remove the process view for now
	rpt.Process = report.MakeTopology()

	/*
	for k, v := range rpt.Process.Nodes {
		//fmt.Println("process:")
		//fmt.Println(k)
		//fmt.Println(v.Parents)
		for _, containerID := range containersToRemove {
			if cVal, ok := v.Parents.Lookup("container"); ok { 
				//fmt.Println(ok)
				//fmt.Println(cVal)
				if containerID == cVal[0] {
					//fmt.Println("removing ", containerID)
					delete(rpt.Process.Nodes, k)
					break
				}
			}
		}
	}*/


	//rpt.Container = report.MakeTopology()
	c.reports = append(c.reports, rpt)
	c.timestamps = append(c.timestamps, mtime.Now())

	c.clean()
	c.cached = nil
	if rpt.Shortcut {
		c.Broadcast()
	}
	return nil
}

// Report returns a merged report over all added reports. It implements
// Reporter.
func (c *collector) Report(_ context.Context) (report.Report, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	// If the oldest report is still within range,
	// and there is a cached report, return that.
	if c.cached != nil && len(c.reports) > 0 {
		oldest := mtime.Now().Add(-c.window)
		if c.timestamps[0].After(oldest) {
			return *c.cached, nil
		}
	}

	c.clean()
	rpt := c.merger.Merge(c.reports).Upgrade()
	c.cached = &rpt
	return rpt, nil
}

func (c *collector) clean() {
	var (
		cleanedReports    = make([]report.Report, 0, len(c.reports))
		cleanedTimestamps = make([]time.Time, 0, len(c.timestamps))
		oldest            = mtime.Now().Add(-c.window)
	)
	for i, r := range c.reports {
		if c.timestamps[i].After(oldest) {
			cleanedReports = append(cleanedReports, r)
			cleanedTimestamps = append(cleanedTimestamps, c.timestamps[i])
		}
	}
	c.reports = cleanedReports
	c.timestamps = cleanedTimestamps
}

// StaticCollector always returns the given report.
type StaticCollector report.Report

// Report returns a merged report over all added reports. It implements
// Reporter.
func (c StaticCollector) Report(context.Context) (report.Report, error) { return report.Report(c), nil }

// Add adds a report to the collector's internal state. It implements Adder.
func (c StaticCollector) Add(context.Context, report.Report, []byte) error { return nil }

// WaitOn lets other components wait on a new report being received. It
// implements Reporter.
func (c StaticCollector) WaitOn(context.Context, chan struct{}) {}

// UnWait lets other components stop waiting on a new report being received. It
// implements Reporter.
func (c StaticCollector) UnWait(context.Context, chan struct{}) {}

// NewFileCollector reads and parses the given path, returning a collector
// which always returns that report.
func NewFileCollector(path string) (Collector, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		rpt     report.Report
		handle  codec.Handle
		gzipped bool
	)
	fileType := filepath.Ext(path)
	if fileType == ".gz" {
		gzipped = true
		fileType = filepath.Ext(strings.TrimSuffix(path, fileType))
	}
	switch fileType {
	case ".json":
		handle = &codec.JsonHandle{}
	case ".msgpack":
		handle = &codec.MsgpackHandle{}
	default:
		return nil, fmt.Errorf("Unsupported file extension: %v", fileType)
	}

	if err := rpt.ReadBinary(f, gzipped, handle); err != nil {
		return nil, err
	}

	return StaticCollector(rpt), nil
}
