/*
 * Copyright (c) 2019 PANTHEON.tech.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package stats

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"git.fd.io/govpp.git"
	"git.fd.io/govpp.git/adapter"
	"git.fd.io/govpp.git/adapter/statsclient"
	"git.fd.io/govpp.git/api"
	"git.fd.io/govpp.git/core"
	"git.fd.io/govpp.git/proxy"
	"github.com/ligato/cn-infra/logging/logrus"

	gre1904 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1904/gre"
	gre1908 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1908/gre"
	gre2001 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001/gre"
	gre2001_324 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001_324/gre"

	gpe1904 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1904/vxlan_gpe"
	gpe1908 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1908/vxlan_gpe"
	gpe2001 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001/vxlan_gpe"
	gpe2001_324 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001_324/vxlan_gpe"

	vpe1904 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1904/vpe"
	vpe1908 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp1908/vpe"
	vpe2001 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001/vpe"
	vpe2001_324 "go.ligato.io/vpp-agent/v2/plugins/vpp/binapi/vpp2001_324/vpe"

	telemetry "go.ligato.io/vpp-agent/v2/plugins/telemetry/vppcalls"
	_ "go.ligato.io/vpp-agent/v2/plugins/telemetry/vppcalls/vpp1904"
	_ "go.ligato.io/vpp-agent/v2/plugins/telemetry/vppcalls/vpp1908"
	_ "go.ligato.io/vpp-agent/v2/plugins/telemetry/vppcalls/vpp2001"
	_ "go.ligato.io/vpp-agent/v2/plugins/telemetry/vppcalls/vpp2001_324"

	ifplugin "go.ligato.io/vpp-agent/v2/plugins/vpp/ifplugin/vppcalls"
	_ "go.ligato.io/vpp-agent/v2/plugins/vpp/ifplugin/vppcalls/vpp1904"
	_ "go.ligato.io/vpp-agent/v2/plugins/vpp/ifplugin/vppcalls/vpp1908"
	_ "go.ligato.io/vpp-agent/v2/plugins/vpp/ifplugin/vppcalls/vpp2001"
	_ "go.ligato.io/vpp-agent/v2/plugins/vpp/ifplugin/vppcalls/vpp2001_324"

	govppmux "go.ligato.io/vpp-agent/v2/plugins/govppmux/vppcalls"
	_ "go.ligato.io/vpp-agent/v2/plugins/govppmux/vppcalls/vpp1904"
	_ "go.ligato.io/vpp-agent/v2/plugins/govppmux/vppcalls/vpp1908"
	_ "go.ligato.io/vpp-agent/v2/plugins/govppmux/vppcalls/vpp2001"
	_ "go.ligato.io/vpp-agent/v2/plugins/govppmux/vppcalls/vpp2001_324"
)

const (
	stateUp   = "up"
	stateDown = "down"
)

var (
	DefaultSocket = adapter.DefaultStatsSocket
)

type (
	VPP struct {
		client    adapter.StatsAPI
		statsConn api.StatsProvider
		vppConn   *core.Connection
		apiChan   api.Channel
		channels  []api.Channel

		// vpp calls
		interfaceHandler ifplugin.InterfaceVppAPI
		telemetryHandler telemetry.TelemetryVppAPI
		govppHandler     govppmux.VpeVppAPI

		version           *govppmux.VersionInfo
		lastErrorCounters map[string]uint64
	}

	Interface struct {
		api.InterfaceCounters
		IPAddrs []string
		State   string
		MTU     []uint32
	}

	ThreadData struct {
		ID        uint32
		Name      []byte
		Type      []byte
		PID       uint32
		CPUID     uint32
		Core      uint32
		CPUSocket uint32
	}
	Node   telemetry.RuntimeItem
	Error  telemetry.NodeCounter
	Memory telemetry.MemoryThread
)

func (s *VPP) ConnectRemote(raddr string) error {
	s.lastErrorCounters = make(map[string]uint64)

	var err error
	var client *proxy.Client
	for i := 0; i < 3; i++ {
		client, err = proxy.Connect(raddr)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("failed to connect to raddr %v, reason: %v", raddr, err)
	}

	s.statsConn, err = client.NewStatsClient()
	if err != nil {
		return err
	}
	s.apiChan, err = client.NewBinapiClient()
	if err != nil {
		return err
	}

	s.channels = make([]api.Channel, 3)
	for i := range s.channels {
		s.channels[i], err = client.NewBinapiClient()
		if err != nil {
			return fmt.Errorf("api channel creation failed: %v", err)
		}
	}

	s.interfaceHandler = ifplugin.CompatibleInterfaceVppHandler(s.channels[0], logrus.NewLogger(""))
	s.telemetryHandler = telemetry.CompatibleTelemetryHandler(s.channels[1], s.statsConn)
	s.govppHandler = govppmux.CompatibleVpeHandler(s.channels[2])

	registerMsgs := func(msgs []api.Message) {
		for _, msg := range msgs {
			gob.Register(msg)
		}
	}

	for ver, h := range govppmux.Versions {
		if err = s.apiChan.CheckCompatiblity(h.Msgs...); err != nil {
			continue
		}
		registerMsgs(h.Msgs)
		registerMsgs(ifplugin.Versions["vpp"+strings.Replace(ver, ".", "", -1)].Msgs)
		registerMsgs(telemetry.Versions[ver].Msgs)

		switch ver {
		case "19.04":
			registerMsgs(gpe1904.AllMessages())
			registerMsgs(gre1904.AllMessages())
		case "19.08":
			registerMsgs(gpe1908.AllMessages())
			registerMsgs(gre1908.AllMessages())
		case "20.01_324":
			registerMsgs(gpe2001_324.AllMessages())
			registerMsgs(gre2001_324.AllMessages())
		case "20.01_379":
			registerMsgs(gpe2001.AllMessages())
			registerMsgs(gre2001.AllMessages())
		}

		break
	}

	s.version, err = s.govppHandler.GetVersionInfo()
	if err != nil {
		return fmt.Errorf("failed to get vpp version: %v", err)
	}

	return nil
}

// Connect establishes a connection to govpp API.
func (s *VPP) Connect(soc string) error {
	s.lastErrorCounters = make(map[string]uint64)

	s.client = statsclient.NewStatsClient(soc)

	var err error
	s.statsConn, err = core.ConnectStats(s.client)
	if err != nil {
		return fmt.Errorf("connection to stats api failed: %v", err)
	}

	s.vppConn, err = govpp.Connect("")
	if err != nil {
		return fmt.Errorf("connection to govpp failed: %v", err)
	}

	s.apiChan, err = s.vppConn.NewAPIChannel()
	if err != nil {
		return err
	}

	s.channels = make([]api.Channel, 3)
	for i := range s.channels {
		s.channels[i], err = s.vppConn.NewAPIChannel()
		if err != nil {
			return fmt.Errorf("api channel creation failed: %v", err)
		}
	}
	s.interfaceHandler = ifplugin.CompatibleInterfaceVppHandler(s.channels[0], logrus.NewLogger(""))
	s.telemetryHandler = telemetry.CompatibleTelemetryHandler(s.channels[1], s.statsConn)
	s.govppHandler = govppmux.CompatibleVpeHandler(s.channels[2])

	s.version, err = s.govppHandler.GetVersionInfo()
	if err != nil {
		return fmt.Errorf("failed to get vpp version: %v", err)
	}

	return nil
}

// Version returns the current vpp version.
func (s *VPP) Version() (string, error) {
	return "VPP version: " + s.version.Version + "\n" + s.version.BuildDate, nil
}

// Disconnect should be called after Connect, if the connection is no longer needed.
func (s *VPP) Disconnect() {
	for _, channel := range s.channels {
		channel.Close()
	}
	s.apiChan.Close()
	if s.vppConn != nil {
		s.vppConn.Disconnect()
	}
	if s.client != nil {
		s.client.Disconnect()
	}
}

// GetNodes returns per node statistics.
func (s *VPP) GetNodes() ([]Node, error) {
	runtimeCounters, err := s.telemetryHandler.GetRuntimeInfo(context.TODO())
	if err != nil {
		return nil, err
	}
	threads := runtimeCounters.GetThreads()
	if len(threads) == 0 {
		return nil, errors.New("No runtime counters")
	}
	result := make([]Node, 0, len(threads[0].Items))
	for _, thread := range threads {
		for _, item := range thread.Items {
			result = append(result, Node(item))
		}
	}
	return result, nil
}

// GetInterfaces returns per interface statistics.
func (s *VPP) GetInterfaces() ([]Interface, error) {
	var ifaceStats *api.InterfaceStats
	var ifaceDetails map[uint32]*ifplugin.InterfaceDetails

	wg := new(sync.WaitGroup)
	wg.Add(2)
	errChan := make(chan error, 2)
	go func() {
		wg.Wait()
		close(errChan)
	}()
	go func() {
		defer wg.Done()
		var err error
		ifaceDetails, err = s.interfaceHandler.DumpInterfaces()
		errChan <- err
	}()
	go func() {
		defer wg.Done()
		var err error
		ifaceStats, err = s.telemetryHandler.GetInterfaceStats(context.TODO())
		errChan <- err
	}()
	for err := range errChan {
		if err != nil {
			return nil, fmt.Errorf("request failed: %v", err)
		}
	}

	result := make([]Interface, 0, len(ifaceDetails))
	for _, iface := range ifaceStats.Interfaces {
		details, ok := ifaceDetails[iface.InterfaceIndex]
		if !ok {
			continue
		}
		state := stateDown
		if details.Interface.GetEnabled() {
			state = stateUp
		}
		result = append(result, Interface{
			InterfaceCounters: iface,
			IPAddrs:           details.Interface.GetIpAddresses(),
			State:             state,
			MTU:               details.Meta.MTU,
		})
	}
	return result, nil
}

// GetErrors returns per error statistics.
func (s *VPP) GetErrors() ([]Error, error) {
	counters, err := s.telemetryHandler.GetNodeCounters(context.TODO())
	if err != nil {
		return nil, err
	}
	result := make([]Error, 0)
	for _, counter := range counters.GetCounters() {
		counter.Value -= s.lastErrorCounters[counter.Node+counter.Name]
		if counter.Value == 0 {
			continue
		}
		result = append(result, Error(counter))
	}
	return result, nil
}

// ClearIfaceCounters resets the counters for the interface.
func (s *VPP) ClearIfaceCounters() error {
	_, err := s.govppHandler.RunCli("clear interfaces")
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}

	return nil
}

// ClearRuntimeCounters clears the runtime counters for nodes.
func (s *VPP) ClearRuntimeCounters() error {
	_, err := s.govppHandler.RunCli("clear runtime")
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	return nil
}

// ClearErrorCounters clears the counters for errors.
func (s *VPP) ClearErrorCounters() error {
	s.updateLastErrors()
	_, err := s.govppHandler.RunCli("clear errors")
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}

	return nil
}

// Memory returns memory usage per thread.
func (s *VPP) Memory() ([]string, error) {
	mem, err := s.govppHandler.RunCli("show memory main-heap verbose")
	if err != nil {
		return nil, err
	}
	rows := make([]string, 0, 1) // there's gonna be at least 1 thread
	for _, r := range strings.Split(mem, "\n") {
		if r == "" {
			continue
		}
		rows = append(rows, strings.Trim(r, " \n"))
	}
	return rows, nil
}

// Threads returns thread data per thread.
func (s *VPP) Threads() ([]ThreadData, error) {
	switch s.version.Release() {
	case "19.04":
		return s.threads1904()
	case "19.08":
		return s.threads1908()
	case "20.01_324":
		return s.threads2001324()
	case "20.01_379":
		return s.threads2001379()
	default:
		return nil, fmt.Errorf("unsuported vpp version")
	}
}

func (s *VPP) threads1904() ([]ThreadData, error) {
	req := &vpe1904.ShowThreads{}
	reply := &vpe1904.ShowThreadsReply{}
	if err := s.apiChan.SendRequest(req).ReceiveReply(reply); err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}

	result := make([]ThreadData, len(reply.ThreadData))
	for i := range reply.ThreadData {
		result[i].ID = reply.ThreadData[i].ID
		result[i].Name = reply.ThreadData[i].Name
		result[i].Type = reply.ThreadData[i].Type
		result[i].PID = reply.ThreadData[i].PID
		result[i].Core = reply.ThreadData[i].Core
		result[i].CPUID = reply.ThreadData[i].CPUID
		result[i].CPUSocket = reply.ThreadData[i].CPUSocket
	}
	return result, nil
}

func (s *VPP) threads1908() ([]ThreadData, error) {
	req := &vpe1908.ShowThreads{}
	reply := &vpe1908.ShowThreadsReply{}
	if err := s.apiChan.SendRequest(req).ReceiveReply(reply); err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}

	result := make([]ThreadData, len(reply.ThreadData))
	for i := range reply.ThreadData {
		result[i].ID = reply.ThreadData[i].ID
		result[i].Name = reply.ThreadData[i].Name
		result[i].Type = reply.ThreadData[i].Type
		result[i].PID = reply.ThreadData[i].PID
		result[i].Core = reply.ThreadData[i].Core
		result[i].CPUID = reply.ThreadData[i].CPUID
		result[i].CPUSocket = reply.ThreadData[i].CPUSocket
	}

	return result, nil
}

func (s *VPP) threads2001324() ([]ThreadData, error) {
	req := &vpe2001_324.ShowThreads{}
	reply := &vpe2001_324.ShowThreadsReply{}
	if err := s.apiChan.SendRequest(req).ReceiveReply(reply); err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}

	result := make([]ThreadData, len(reply.ThreadData))
	for i := range reply.ThreadData {
		result[i].ID = reply.ThreadData[i].ID
		result[i].Name = reply.ThreadData[i].Name
		result[i].Type = reply.ThreadData[i].Type
		result[i].PID = reply.ThreadData[i].PID
		result[i].Core = reply.ThreadData[i].Core
		result[i].CPUID = reply.ThreadData[i].CPUID
		result[i].CPUSocket = reply.ThreadData[i].CPUSocket
	}

	return result, nil
}

func (s *VPP) threads2001379() ([]ThreadData, error) {
	req := &vpe2001.ShowThreads{}
	reply := &vpe2001.ShowThreadsReply{}
	if err := s.apiChan.SendRequest(req).ReceiveReply(reply); err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}

	result := make([]ThreadData, len(reply.ThreadData))
	for i := range reply.ThreadData {
		result[i].ID = reply.ThreadData[i].ID
		result[i].Name = reply.ThreadData[i].Name
		result[i].Type = reply.ThreadData[i].Type
		result[i].PID = reply.ThreadData[i].PID
		result[i].Core = reply.ThreadData[i].Core
		result[i].CPUID = reply.ThreadData[i].CPUID
		result[i].CPUSocket = reply.ThreadData[i].CPUSocket
	}

	return result, nil
}

func (s *VPP) updateLastErrors() {
	counters, err := s.telemetryHandler.GetNodeCounters(context.TODO())
	if err != nil {
		return
	}

	for _, counter := range counters.GetCounters() {
		if counter.Value == 0 {
			continue
		}
		s.lastErrorCounters[counter.Node+counter.Name] = counter.Value
	}
}
