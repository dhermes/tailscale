// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tsdial provides a Dialer type that can dial out of tailscaled.
package tsdial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"inet.af/netaddr"
	"tailscale.com/net/netknob"
)

// Dialer dials out of tailscaled, while taking care of details while
// handling the dozens of edge cases depending on the server mode
// (TUN, netstack), the OS network sandboxing style (macOS/iOS
// Extension, none), user-selected route acceptance prefs, etc.
type Dialer struct {
	// UseNetstackForIP if non-nil is whether NetstackDialTCP (if
	// it's non-nil) should be used to dial the provided IP.
	UseNetstackForIP func(netaddr.IP) bool

	// NetstackDialTCP dials the provided IPPort using netstack.
	// If nil, it's not used.
	NetstackDialTCP func(context.Context, netaddr.IPPort) (net.Conn, error)

	peerDialControlFuncAtomic atomic.Value // of func() func(network, address string, c syscall.RawConn) error

	peerClientOnce sync.Once
	peerClient     *http.Client

	mu  sync.Mutex
	dns DNSMap
}

// PeerDialControlFunc returns a function
// that can assigned to net.Dialer.Control to set sockopts or whatnot
// to make a dial escape the current platform's network sandbox.
//
// On many platforms the returned func will be nil.
//
// Notably, this is non-nil on iOS and macOS when run as a Network or
// System Extension (the GUI variants).
func (d *Dialer) PeerDialControlFunc() func(network, address string, c syscall.RawConn) error {
	gf, _ := d.peerDialControlFuncAtomic.Load().(func() func(network, address string, c syscall.RawConn) error)
	if gf == nil {
		return nil
	}
	return gf()
}

// SetPeerDialControlFuncGetter sets a function that returns, for the
// current network configuration at the time it's called, a function
// that can assigned to net.Dialer.Control to set sockopts or whatnot
// to make a dial escape the current platform's network sandbox.
func (d *Dialer) SetPeerDialControlFuncGetter(getFunc func() func(network, address string, c syscall.RawConn) error) {
	d.peerDialControlFuncAtomic.Store(getFunc)
}

func (d *Dialer) SetDNSMap(m DNSMap) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dns = m
}

func (d *Dialer) resolve(ctx context.Context, addr string) (netaddr.IPPort, error) {
	d.mu.Lock()
	dns := d.dns
	d.mu.Unlock()
	return dns.Resolve(ctx, addr)
}

// UserDial connects to the provided network address as if a user were initiating the dial.
// (e.g. from a SOCKS or HTTP outbound proxy)
func (d *Dialer) UserDial(ctx context.Context, network, addr string) (net.Conn, error) {
	ipp, err := d.resolve(ctx, addr)
	if err != nil {
		return nil, err
	}
	if d.UseNetstackForIP != nil && d.UseNetstackForIP(ipp.IP()) {
		if d.NetstackDialTCP == nil {
			return nil, errors.New("Dialer not initialized correctly")
		}
		return d.NetstackDialTCP(ctx, ipp)
	}
	// TODO(bradfitz): netns, etc
	var stdDialer net.Dialer
	return stdDialer.DialContext(ctx, network, ipp.String())
}

func (d *Dialer) PeerAPIDial(ctx context.Context, ip netaddr.IP) (net.Conn, error) {
	panic("TODO")
}

func (d *Dialer) PeerAPIHTTPClient() *http.Client {
	d.peerClientOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Dial = nil
		dialer := net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: netknob.PlatformTCPKeepAlive(),
			Control:   d.PeerDialControlFunc(),
		}
		t.DialContext = dialer.DialContext
		d.peerClient = &http.Client{Transport: t}
	})
	return d.peerClient
}

// PeerAPITransport returns a Transport to dial peers' peerapi endpoints.
//
// The returned value must not be mutated; it's owned by the Dialer
// and shared by callers.
func (d *Dialer) PeerAPITransport() *http.Transport {
	return d.PeerAPIHTTPClient().Transport.(*http.Transport)
}
