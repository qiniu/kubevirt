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
 */

package netpod_test

import (
	"fmt"

	vishnetlink "github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/network/driver/nmstate"
	"kubevirt.io/kubevirt/pkg/network/setup/netpod"
	"kubevirt.io/kubevirt/pkg/pointer"
)

var _ = Describe("netpod bandwidth", func() {
	const tapName = "tap0"

	var (
		state *netpod.State
		tc    *tcStub
	)

	BeforeEach(func() {
		state = netpod.NewState(newConfigStateCacheStub(), netnsStub{})
		tc = newTCStub(tapName)
	})

	newNetPodWithBandwidth := func(binding v1.InterfaceBindingMethod, bandwidth *v1.Bandwidth) netpod.NetPod {
		return netpod.NewNetPod(
			[]v1.Network{*v1.DefaultPodNetwork()},
			[]v1.Interface{{
				Name:                   defaultPodNetworkName,
				InterfaceBindingMethod: binding,
				Bandwidth:              bandwidth,
			}},
			vmiUID, 0, 0, 0, state,
			netpod.WithNMStateAdapter(&nmstateStub{status: nmstate.Status{
				Interfaces: []nmstate.Interface{{Name: "eth0", Index: 1}},
			}}),
			netpod.WithTCAdapter(tc),
		)
	}

	masquerade := v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}}

	It("does nothing when no interface requests bandwidth", func() {
		netPod := newNetPodWithBandwidth(masquerade, nil)

		Expect(netPod.SyncBandwidth()).To(Succeed())
		Expect(tc.replacedQdiscs).To(BeEmpty())
		Expect(tc.replacedFilters).To(BeEmpty())
		Expect(tc.deletedQdiscs).To(BeEmpty())
	})

	It("shapes traffic towards the guest with a tbf root qdisc", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound: &v1.BandwidthParams{
				Average: pointer.P(uint32(12500)),
				Peak:    pointer.P(uint32(15000)),
				Burst:   pointer.P(uint32(2048)),
			},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		Expect(tc.replacedQdiscs).To(HaveLen(1))
		tbf, ok := tc.replacedQdiscs[0].(*vishnetlink.Tbf)
		Expect(ok).To(BeTrue(), "expected a tbf qdisc, got %q", tc.replacedQdiscs[0].Type())

		Expect(tbf.Attrs().Parent).To(Equal(uint32(vishnetlink.HANDLE_ROOT)))
		Expect(tbf.Attrs().LinkIndex).To(Equal(tapLinkIndex))
		// KiB/s and KiB from the API are converted to the bytes tc expects.
		Expect(tbf.Rate).To(Equal(uint64(12500 * 1024)))
		Expect(tbf.Peakrate).To(Equal(uint64(15000 * 1024)))
		Expect(tbf.Buffer).To(Equal(uint32(2048 * 1024)))
		// limit = rate * 50ms + buffer
		Expect(tbf.Limit).To(Equal(uint32(12500*1024/1000*50 + 2048*1024)))
		Expect(tbf.Minburst).ToNot(BeZero(), "peakrate requires a burst bucket")
	})

	It("omits peakrate when it is not requested", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound: &v1.BandwidthParams{
				Average: pointer.P(uint32(1000)),
				Burst:   pointer.P(uint32(64)),
			},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		tbf := tc.replacedQdiscs[0].(*vishnetlink.Tbf)
		Expect(tbf.Peakrate).To(BeZero())
		Expect(tbf.Minburst).To(BeZero())
	})

	It("polices traffic sent by the guest on the ingress path", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Outbound: &v1.BandwidthParams{
				Average: pointer.P(uint32(6250)),
				Peak:    pointer.P(uint32(7500)),
				Burst:   pointer.P(uint32(1024)),
			},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		Expect(tc.replacedQdiscs).To(HaveLen(1))
		Expect(tc.replacedQdiscs[0].Type()).To(Equal("ingress"))

		Expect(tc.replacedFilters).To(HaveLen(1))
		matchAll, ok := tc.replacedFilters[0].(*vishnetlink.MatchAll)
		Expect(ok).To(BeTrue(), "expected a matchall filter, got %q", tc.replacedFilters[0].Type())
		Expect(matchAll.Attrs().Parent).To(Equal(uint32(vishnetlink.HANDLE_INGRESS)))
		Expect(matchAll.Actions).To(HaveLen(1))

		police, ok := matchAll.Actions[0].(*vishnetlink.PoliceAction)
		Expect(ok).To(BeTrue())
		Expect(police.Rate).To(Equal(uint32(6250 * 1024)))
		Expect(police.PeakRate).To(Equal(uint32(7500 * 1024)))
		Expect(police.Burst).To(Equal(uint32(1024 * 1024)))
		Expect(police.ExceedAction).To(Equal(vishnetlink.TC_POLICE_SHOT))
	})

	It("configures both directions at once", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound:  &v1.BandwidthParams{Average: pointer.P(uint32(100)), Burst: pointer.P(uint32(10))},
			Outbound: &v1.BandwidthParams{Average: pointer.P(uint32(200)), Burst: pointer.P(uint32(20))},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		Expect(tc.replacedQdiscs).To(HaveLen(2))
		Expect(tc.replacedQdiscs[0].Type()).To(Equal("tbf"))
		Expect(tc.replacedQdiscs[1].Type()).To(Equal("ingress"))
		Expect(tc.replacedFilters).To(HaveLen(1))
	})

	// Removing the bandwidth stanza has to actually lift the limit, otherwise the
	// guest stays throttled with no way back short of a restart.
	It("removes a previously configured shaper when bandwidth is dropped", func() {
		tc.existingQdiscs = []vishnetlink.Qdisc{
			&vishnetlink.Tbf{QdiscAttrs: vishnetlink.QdiscAttrs{LinkIndex: tapLinkIndex, Parent: vishnetlink.HANDLE_ROOT}},
			&vishnetlink.Ingress{QdiscAttrs: vishnetlink.QdiscAttrs{LinkIndex: tapLinkIndex, Parent: vishnetlink.HANDLE_INGRESS}},
		}

		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		Expect(tc.replacedQdiscs).To(BeEmpty())
		Expect(tc.deletedQdiscs).To(HaveLen(2))
		Expect(tc.deletedQdiscs[0].Type()).To(Equal("tbf"))
		Expect(tc.deletedQdiscs[1].Type()).To(Equal("ingress"))
	})

	It("keeps the other direction when only one is dropped", func() {
		tc.existingQdiscs = []vishnetlink.Qdisc{
			&vishnetlink.Ingress{QdiscAttrs: vishnetlink.QdiscAttrs{LinkIndex: tapLinkIndex, Parent: vishnetlink.HANDLE_INGRESS}},
		}

		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound: &v1.BandwidthParams{Average: pointer.P(uint32(100)), Burst: pointer.P(uint32(10))},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		Expect(tc.replacedQdiscs).To(HaveLen(1))
		Expect(tc.replacedQdiscs[0].Type()).To(Equal("tbf"))
		Expect(tc.deletedQdiscs).To(HaveLen(1))
		Expect(tc.deletedQdiscs[0].Type()).To(Equal("ingress"))
	})

	It("does not touch a qdisc it did not create", func() {
		tc.existingQdiscs = []vishnetlink.Qdisc{
			&vishnetlink.Htb{QdiscAttrs: vishnetlink.QdiscAttrs{LinkIndex: tapLinkIndex, Parent: vishnetlink.HANDLE_ROOT}},
		}

		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{})

		Expect(netPod.SyncBandwidth()).To(Succeed())
		Expect(tc.deletedQdiscs).To(BeEmpty())
	})

	// The API takes uint32 KiB, so the conversion to bytes can exceed what the
	// uint32 tc fields hold. Wrapping would silently turn a huge limit into a tiny one.
	It("clamps values that overflow the kernel fields", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Outbound: &v1.BandwidthParams{
				Average: pointer.P(uint32(1 << 30)),
				Peak:    pointer.P(uint32(1 << 31)),
				Burst:   pointer.P(uint32(1 << 30)),
			},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		police := tc.replacedFilters[0].(*vishnetlink.MatchAll).Actions[0].(*vishnetlink.PoliceAction)
		Expect(police.Rate).To(Equal(uint32(0xFFFFFFFF)))
		Expect(police.PeakRate).To(Equal(uint32(0xFFFFFFFF)))
		Expect(police.Burst).To(Equal(uint32(0xFFFFFFFF)))
	})

	It("does not wrap the tbf limit at the maximum rate", func() {
		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound: &v1.BandwidthParams{
				Average: pointer.P(uint32(0xFFFFFFFF)),
				Burst:   pointer.P(uint32(0xFFFFFFFF)),
			},
		})

		Expect(netPod.SyncBandwidth()).To(Succeed())

		tbf := tc.replacedQdiscs[0].(*vishnetlink.Tbf)
		Expect(tbf.Rate).To(Equal(uint64(0xFFFFFFFF) * 1024))
		Expect(tbf.Buffer).To(Equal(uint32(0xFFFFFFFF)))
		Expect(tbf.Limit).To(Equal(uint32(0xFFFFFFFF)))
	})

	It("skips bindings that have no tap device", func() {
		netPod := newNetPodWithBandwidth(
			v1.InterfaceBindingMethod{SRIOV: &v1.InterfaceSRIOV{}},
			&v1.Bandwidth{Inbound: &v1.BandwidthParams{Average: pointer.P(uint32(100)), Burst: pointer.P(uint32(10))}},
		)

		Expect(netPod.SyncBandwidth()).To(Succeed())
		Expect(tc.replacedQdiscs).To(BeEmpty())
	})

	It("reports an error when the tap device is missing", func() {
		tc.linkErr = fmt.Errorf("link not found")

		netPod := newNetPodWithBandwidth(masquerade, &v1.Bandwidth{
			Inbound: &v1.BandwidthParams{Average: pointer.P(uint32(100)), Burst: pointer.P(uint32(10))},
		})

		Expect(netPod.SyncBandwidth()).To(MatchError(ContainSubstring("link not found")))
	})
})

const tapLinkIndex = 7

type tcStub struct {
	tapName        string
	linkErr        error
	existingQdiscs []vishnetlink.Qdisc

	replacedQdiscs  []vishnetlink.Qdisc
	deletedQdiscs   []vishnetlink.Qdisc
	replacedFilters []vishnetlink.Filter
}

func newTCStub(tapName string) *tcStub {
	return &tcStub{tapName: tapName}
}

func (t *tcStub) LinkByName(name string) (vishnetlink.Link, error) {
	if t.linkErr != nil {
		return nil, t.linkErr
	}
	if name != t.tapName {
		return nil, fmt.Errorf("unexpected link %q", name)
	}
	return &vishnetlink.GenericLink{
		LinkAttrs: vishnetlink.LinkAttrs{Name: name, Index: tapLinkIndex},
		LinkType:  "tun",
	}, nil
}

func (t *tcStub) QdiscList(_ vishnetlink.Link) ([]vishnetlink.Qdisc, error) {
	return t.existingQdiscs, nil
}

func (t *tcStub) QdiscReplace(qdisc vishnetlink.Qdisc) error {
	t.replacedQdiscs = append(t.replacedQdiscs, qdisc)
	return nil
}

func (t *tcStub) QdiscDel(qdisc vishnetlink.Qdisc) error {
	t.deletedQdiscs = append(t.deletedQdiscs, qdisc)
	return nil
}

func (t *tcStub) FilterReplace(filter vishnetlink.Filter) error {
	t.replacedFilters = append(t.replacedFilters, filter)
	return nil
}
