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

package netlink

import (
	"github.com/vishvananda/netlink"
)

func (n NetLink) QdiscList(link netlink.Link) ([]netlink.Qdisc, error) {
	return netlink.QdiscList(link)
}

func (n NetLink) QdiscReplace(qdisc netlink.Qdisc) error {
	return netlink.QdiscReplace(qdisc)
}

func (n NetLink) QdiscDel(qdisc netlink.Qdisc) error {
	return netlink.QdiscDel(qdisc)
}

func (n NetLink) FilterReplace(filter netlink.Filter) error {
	return netlink.FilterReplace(filter)
}
