package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/parca-dev/parca-agent/ksym"
	profilestorepb "github.com/parca-dev/parca/proto/gen/go/profilestore"
)

type SystemdManager struct {
	logger          log.Logger
	nodeName        string
	samplingRatio   float64
	ksymCache       *ksym.KsymCache
	writeClient     profilestorepb.ProfileStoreClient
	debugInfoClient DebugInfoClient
	sink            func(Record)
	units           map[string]struct{}
	unitProfilers   map[string]*CgroupProfiler
	mtx             *sync.RWMutex
}

type SystemdUnitTarget struct {
	Name     string
	NodeName string
}

func (t *SystemdUnitTarget) Labels() []*profilestorepb.Label {
	return []*profilestorepb.Label{{
		Name:  "node",
		Value: t.NodeName,
	}, {
		Name:  "systemd_unit",
		Value: t.Name,
	}}
}

func (t *SystemdUnitTarget) PerfEventCgroupPath() string {
	return fmt.Sprintf("/sys/fs/cgroup/perf_event/system.slice/%s/", t.Name)
}

func NewSystemdManager(
	logger log.Logger,
	nodeName string,
	units []string,
	samplingRatio float64,
	ksymCache *ksym.KsymCache,
	writeClient profilestorepb.ProfileStoreClient,
	debugInfoClient DebugInfoClient,
) *SystemdManager {
	unitsSet := map[string]struct{}{}

	for _, unit := range units {
		unitsSet[unit] = struct{}{}
	}

	return &SystemdManager{
		logger:          logger,
		nodeName:        nodeName,
		samplingRatio:   samplingRatio,
		ksymCache:       ksymCache,
		writeClient:     writeClient,
		debugInfoClient: debugInfoClient,
		mtx:             &sync.RWMutex{},
		units:           unitsSet,
		unitProfilers:   map[string]*CgroupProfiler{},
	}
}

func (m *SystemdManager) SetSink(sink func(Record)) {
	m.sink = sink
}

func (m *SystemdManager) ActiveProfilers() []Profiler {
	names := []string{}
	for unit, _ := range m.units {
		names = append(names, unit)
	}
	sort.Strings(names)

	res := []Profiler{}
	for _, name := range names {
		res = append(res, m.unitProfilers[name])
	}

	return res
}

func (m *SystemdManager) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		level.Debug(m.logger).Log("msg", "running systemd manager", "units", len(m.units))
		for unit, _ := range m.units {
			if err := m.reconcileUnit(ctx, unit); err != nil {
				return err
			}
		}
	}
}

func (m *SystemdManager) reconcileUnit(ctx context.Context, unit string) error {
	f, err := os.Open(fmt.Sprintf("/sys/fs/cgroup/systemd/system.slice/%s/cgroup.procs", unit))
	if os.IsNotExist(err) {
		m.mtx.Lock()
		p := m.unitProfilers[unit]
		if p != nil {
			p.Stop()
		}
		m.unitProfilers[unit] = nil
		m.mtx.Unlock()
		//TODO(brancz): cleanup cgroup os.Remove(fmt.Sprintf("/sys/fs/cgroup/perf_event/system.slice/%s/")
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	err = os.MkdirAll(fmt.Sprintf("/sys/fs/cgroup/perf_event/system.slice/%s/", unit), os.ModePerm)
	if err != nil {
		return err
	}

	s := bufio.NewScanner(f)
	for s.Scan() {
		if err := retryingWriteFile(
			fmt.Sprintf("/sys/fs/cgroup/perf_event/system.slice/%s/cgroup.procs", unit),
			[]byte(s.Text()),
			os.FileMode(0),
		); err != nil {
			return err
		}
	}
	if err := s.Err(); err != nil {
		return err
	}

	m.mtx.RLock()
	_, exists := m.unitProfilers[unit]
	m.mtx.RUnlock()
	if exists {
		// profiler already running for this cgroup
		return nil
	}

	logger := log.With(m.logger, "systemdunit", unit)
	p := NewCgroupProfiler(
		logger,
		m.ksymCache,
		m.writeClient,
		m.debugInfoClient,
		&SystemdUnitTarget{
			Name:     unit,
			NodeName: m.nodeName,
		},
		m.sink,
	)

	level.Debug(logger).Log("msg", "adding systemd unit profiler")
	m.mtx.Lock()
	m.unitProfilers[unit] = p
	m.mtx.Unlock()

	go func() {
		err := p.Run(ctx)
		if err != nil {
			level.Error(m.logger).Log("msg", "running systemd-unit profiler failed", "err", err)
		}
	}()

	return nil
}

func retryingWriteFile(path string, data []byte, mode os.FileMode) error {
	// Retry writes on EINTR; see:
	//    https://github.com/golang/go/issues/38033
	for {
		err := ioutil.WriteFile(path, data, mode)
		if err == nil {
			return nil
		} else if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}