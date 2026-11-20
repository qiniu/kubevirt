/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package netpod

import (
	"fmt"
	"math"

	vishnetlink "github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	k8serrors "k8s.io/apimachinery/pkg/util/errors"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/network/link"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
)

// Interface bandwidth (QoS) is enforced with tc on the tap device, inside the
// virt-launcher pod network namespace. libvirt can do this on its own, but only
// when it runs in system mode. Non-root virt-launcher runs a session mode
// libvirt which rejects <bandwidth> outright, so virt-handler (privileged)
// applies it instead. This keeps QoS working regardless of the Root feature gate.
//
// Direction note: `inbound` is traffic entering the guest, which leaves the host
// through the tap egress path, hence the root qdisc. `outbound` is traffic the
// guest sends, which arrives on the tap ingress path and is policed there.
// This mirrors what libvirt does in virNetDevBandwidthSet().
const (
	// tbfLatencyMilliSec bounds the time a packet may sit in the shaper queue.
	// It matches the "latency 50ms" libvirt passes to tc.
	tbfLatencyMilliSec = 50

	// tbfMinBurstBytes is the peakrate bucket size, equivalent to tc's "mtu"
	// argument. It must be at least one MTU or the kernel rejects the qdisc.
	tbfMinBurstBytes = 1600

	bytesPerKiB = 1024
)

type tcAdapter interface {
	LinkByName(name string) (vishnetlink.Link, error)
	QdiscList(link vishnetlink.Link) ([]vishnetlink.Qdisc, error)
	QdiscReplace(qdisc vishnetlink.Qdisc) error
	QdiscDel(qdisc vishnetlink.Qdisc) error
	FilterReplace(filter vishnetlink.Filter) error
}

// SyncBandwidth reconciles the tc configuration of every tap device with the
// bandwidth requested in the VMI spec. It is idempotent: it may be called on
// every sync iteration and performs no action when nothing changed.
func (n NetPod) SyncBandwidth() error {
	ifaces := n.bandwidthManagedInterfaces()
	if len(ifaces) == 0 {
		return nil
	}

	return n.state.NSExec.Do(func() error {
		currentStatus, err := n.nmstateAdapter.Read()
		if err != nil {
			return err
		}
		podIfaceNameByNetwork := createNetworkNameScheme(n.vmiSpecNets, n.vmiIfaceStatuses, currentStatus.Interfaces)

		var errs []error
		for _, iface := range ifaces {
			vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, iface.Name)
			podIfaceName := podIfaceNameByNetwork[iface.Name]
			if vmiNetwork == nil || podIfaceName == "" {
				continue
			}

			tapName := link.GenerateTapDeviceName(podIfaceName, *vmiNetwork)
			if err := n.syncTapBandwidth(tapName, iface.Bandwidth); err != nil {
				errs = append(errs, fmt.Errorf("failed to sync bandwidth on %s (interface %s): %w", tapName, iface.Name, err))
			}
		}
		return k8serrors.NewAggregate(errs)
	})
}

// bandwidthManagedInterfaces returns the interfaces whose traffic flows through a
// tap device. Bindings without a tap (SR-IOV, passt) cannot be shaped this way.
func (n NetPod) bandwidthManagedInterfaces() []v1.Interface {
	var ifaces []v1.Interface
	for _, iface := range n.vmiSpecIfaces {
		if iface.State == v1.InterfaceStateAbsent {
			continue
		}

		hasTap := iface.Bridge != nil || iface.Masquerade != nil
		if iface.Binding != nil {
			bindingPlugin, exists := n.bindingPluginsByName[iface.Binding.Name]
			hasTap = exists && bindingPlugin.DomainAttachmentType == v1.ManagedTap
		}
		if !hasTap {
			continue
		}

		ifaces = append(ifaces, iface)
	}
	return ifaces
}

func (n NetPod) syncTapBandwidth(tapName string, bandwidth *v1.Bandwidth) error {
	tapLink, err := n.tcAdapter.LinkByName(tapName)
	if err != nil {
		// The tap may legitimately not exist yet, e.g. an interface being hotplugged.
		// Reporting it lets the caller retry on the next sync.
		return fmt.Errorf("failed to lookup tap device: %w", err)
	}
	linkIndex := tapLink.Attrs().Index

	var inbound, outbound *v1.BandwidthParams
	if bandwidth != nil {
		inbound, outbound = bandwidth.Inbound, bandwidth.Outbound
	}

	if err := n.syncInbound(tapLink, linkIndex, inbound); err != nil {
		return err
	}
	return n.syncOutbound(tapLink, linkIndex, outbound)
}

// syncInbound shapes traffic towards the guest using a tbf root qdisc.
func (n NetPod) syncInbound(tapLink vishnetlink.Link, linkIndex int, params *v1.BandwidthParams) error {
	if params == nil {
		return n.deleteRootTbf(tapLink)
	}
	return n.tcAdapter.QdiscReplace(newTbf(linkIndex, params))
}

func (n NetPod) deleteRootTbf(tapLink vishnetlink.Link) error {
	qdiscs, err := n.tcAdapter.QdiscList(tapLink)
	if err != nil {
		return fmt.Errorf("failed to list qdiscs: %w", err)
	}
	for _, qdisc := range qdiscs {
		if qdisc.Type() != "tbf" || qdisc.Attrs().Parent != vishnetlink.HANDLE_ROOT {
			continue
		}
		// Removing the root qdisc restores the kernel default (pfifo_fast).
		if err := n.tcAdapter.QdiscDel(qdisc); err != nil {
			return fmt.Errorf("failed to delete tbf qdisc: %w", err)
		}
	}
	return nil
}

// syncOutbound polices traffic coming from the guest on the tap ingress path.
func (n NetPod) syncOutbound(tapLink vishnetlink.Link, linkIndex int, params *v1.BandwidthParams) error {
	if params == nil {
		return n.deleteIngress(tapLink)
	}

	ingress := &vishnetlink.Ingress{
		QdiscAttrs: vishnetlink.QdiscAttrs{
			LinkIndex: linkIndex,
			Handle:    vishnetlink.MakeHandle(0xffff, 0),
			Parent:    vishnetlink.HANDLE_INGRESS,
		},
	}
	if err := n.tcAdapter.QdiscReplace(ingress); err != nil {
		return fmt.Errorf("failed to replace ingress qdisc: %w", err)
	}

	return n.tcAdapter.FilterReplace(newPoliceFilter(linkIndex, params))
}

func (n NetPod) deleteIngress(tapLink vishnetlink.Link) error {
	qdiscs, err := n.tcAdapter.QdiscList(tapLink)
	if err != nil {
		return fmt.Errorf("failed to list qdiscs: %w", err)
	}
	for _, qdisc := range qdiscs {
		if qdisc.Type() != "ingress" {
			continue
		}
		// Deleting the ingress qdisc drops the attached police filter with it.
		if err := n.tcAdapter.QdiscDel(qdisc); err != nil {
			return fmt.Errorf("failed to delete ingress qdisc: %w", err)
		}
	}
	return nil
}

func newTbf(linkIndex int, params *v1.BandwidthParams) *vishnetlink.Tbf {
	rate := kiBPerSecToBytesPerSec(params.Average)
	buffer := kiBToBytes(params.Burst)

	tbf := &vishnetlink.Tbf{
		QdiscAttrs: vishnetlink.QdiscAttrs{
			LinkIndex: linkIndex,
			Handle:    vishnetlink.MakeHandle(1, 0),
			Parent:    vishnetlink.HANDLE_ROOT,
		},
		Rate:   rate,
		Buffer: clampToUint32(buffer),
		// Keep the multiplication in milliseconds: rate is already in bytes and
		// scaling it by a nanosecond latency overflows uint64 for large rates.
		Limit: clampToUint32(rate/1000*tbfLatencyMilliSec + buffer),
	}

	if peak := kiBPerSecToBytesPerSec(params.Peak); peak > 0 {
		tbf.Peakrate = peak
		tbf.Minburst = tbfMinBurstBytes
	}

	return tbf
}

func newPoliceFilter(linkIndex int, params *v1.BandwidthParams) *vishnetlink.MatchAll {
	police := vishnetlink.NewPoliceAction()
	police.Rate = clampToUint32(kiBPerSecToBytesPerSec(params.Average))
	police.Burst = clampToUint32(kiBToBytes(params.Burst))
	police.PeakRate = clampToUint32(kiBPerSecToBytesPerSec(params.Peak))
	// Without an explicit mtu the kernel cannot size the peakrate bucket.
	police.Mtu = tbfMinBurstBytes
	police.ExceedAction = vishnetlink.TC_POLICE_SHOT

	return &vishnetlink.MatchAll{
		FilterAttrs: vishnetlink.FilterAttrs{
			LinkIndex: linkIndex,
			Parent:    vishnetlink.HANDLE_INGRESS,
			Handle:    vishnetlink.MakeHandle(0, 1),
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []vishnetlink.Action{police},
	}
}

func kiBPerSecToBytesPerSec(kiBPerSec *uint32) uint64 {
	return kiBToBytes(kiBPerSec)
}

func kiBToBytes(kiB *uint32) uint64 {
	if kiB == nil {
		return 0
	}
	return uint64(*kiB) * bytesPerKiB
}

// clampToUint32 keeps the kernel from receiving a wrapped value. The API accepts
// uint32 KiB, so converting to bytes can overflow the uint32 fields tc uses.
func clampToUint32(value uint64) uint32 {
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}
