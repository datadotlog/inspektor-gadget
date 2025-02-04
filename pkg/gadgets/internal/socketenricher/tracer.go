// Copyright 2022 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package socketenricher

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/kallsyms"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target $TARGET -cc clang socketenricher ./bpf/sockets-map.bpf.c -- -I./bpf/ -I../../../ -I../../../${TARGET}

// SocketEnricher creates a map exposing processes owning each socket.
//
// This makes it possible for network gadgets to access that information and
// display it directly from the BPF code. Example of such code in the dns and
// sni gadgets.
type SocketEnricher struct {
	objs  socketenricherObjects
	links []link.Link

	closeOnce sync.Once
	done      chan bool
}

func (se *SocketEnricher) SocketsMap() *ebpf.Map {
	return se.objs.Sockets
}

func NewSocketEnricher() (*SocketEnricher, error) {
	se := &SocketEnricher{}

	if err := se.start(); err != nil {
		se.Close()
		return nil, err
	}

	return se, nil
}

func (se *SocketEnricher) start() error {
	spec, err := loadSocketenricher()
	if err != nil {
		return fmt.Errorf("loading asset: %w", err)
	}

	kallsyms.SpecUpdateAddresses(spec, []string{"socket_file_ops"})

	if err := spec.LoadAndAssign(&se.objs, nil); err != nil {
		return fmt.Errorf("loading ebpf program: %w", err)
	}

	var l link.Link

	// bind
	l, err = link.Kprobe("inet_bind", se.objs.IgBindIpv4E, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv4 kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kretprobe("inet_bind", se.objs.IgBindIpv4X, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv4 kretprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kprobe("inet6_bind", se.objs.IgBindIpv6E, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv6 kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kretprobe("inet6_bind", se.objs.IgBindIpv6X, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv6 kretprobe: %w", err)
	}
	se.links = append(se.links, l)

	// connect
	l, err = link.Kprobe("tcp_v4_connect", se.objs.IgTcpcV4CoE, nil)
	if err != nil {
		return fmt.Errorf("attaching connect ipv4 kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kretprobe("tcp_v4_connect", se.objs.IgTcpcV4CoX, nil)
	if err != nil {
		return fmt.Errorf("attaching connect ipv4 kretprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kprobe("tcp_v6_connect", se.objs.IgTcpcV6CoE, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv6 connect kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kretprobe("tcp_v6_connect", se.objs.IgTcpcV6CoX, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv6 connect kretprobe: %w", err)
	}
	se.links = append(se.links, l)

	// udp_sendmsg
	l, err = link.Kprobe("udp_sendmsg", se.objs.IgUdpSendmsg, nil)
	if err != nil {
		return fmt.Errorf("attaching udp_sendmsg ipv4 kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kprobe("udpv6_sendmsg", se.objs.IgUdp6Sendmsg, nil)
	if err != nil {
		return fmt.Errorf("attaching udpv6_sendmsg ipv6 kprobe: %w", err)
	}
	se.links = append(se.links, l)

	// release
	l, err = link.Kprobe("inet_release", se.objs.IgFreeIpv4E, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv4 release kprobe: %w", err)
	}
	se.links = append(se.links, l)

	l, err = link.Kprobe("inet6_release", se.objs.IgFreeIpv6E, nil)
	if err != nil {
		return fmt.Errorf("attaching ipv6 release kprobe: %w", err)
	}
	se.links = append(se.links, l)

	// get initial sockets
	socketsIter, err := link.AttachIter(link.IterOptions{
		Program: se.objs.IgSocketsIt,
	})
	if err != nil {
		return fmt.Errorf("attach BPF iterator: %w", err)
	}
	defer socketsIter.Close()

	file, err := socketsIter.Open()
	if err != nil {
		return fmt.Errorf("open BPF iterator: %w", err)
	}
	defer file.Close()
	_, err = io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read BPF iterator: %w", err)
	}

	cleanupIter, err := link.AttachIter(link.IterOptions{
		Program: se.objs.IgSkCleanup,
		Map:     se.objs.Sockets,
	})
	if err != nil {
		return fmt.Errorf("attach BPF iterator for cleanups: %w", err)
	}
	se.links = append(se.links, cleanupIter)

	se.done = make(chan bool)
	go se.cleanupDeletedSockets(cleanupIter)

	return nil
}

func (se *SocketEnricher) cleanupDeletedSockets(cleanupIter *link.Iter) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-se.done:
			return
		case <-ticker.C:
			err := se.cleanupDeletedSocketsNow(cleanupIter)
			if err != nil {
				fmt.Printf("socket enricher: %v\n", err)
			}
		}
	}
}

func (se *SocketEnricher) cleanupDeletedSocketsNow(cleanupIter *link.Iter) error {
	file, err := cleanupIter.Open()
	if err != nil {
		return fmt.Errorf("open BPF iterator: %w", err)
	}
	defer file.Close()
	_, err = io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read BPF iterator: %w", err)
	}
	return nil
}

func (se *SocketEnricher) Close() {
	se.closeOnce.Do(func() {
		if se.done != nil {
			close(se.done)
		}
	})

	for _, l := range se.links {
		gadgets.CloseLink(l)
	}
	se.links = nil
	se.objs.Close()
}
