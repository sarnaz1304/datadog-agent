// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	netstats "github.com/DataDog/datadog-agent/pkg/network/stats"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/golang/groupcache/lru"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type routeKey struct {
	source, dest util.Address
	netns        uint32
	connFamily   ConnectionFamily
}

// Route stores info for a route table entry
type Route struct {
	Gateway util.Address
	IfIndex int
}

type routeTTL struct {
	eta   int64
	entry Route
}

type routeCache struct {
	sync.Mutex
	cache  *lru.Cache
	router Router
	ttl    time.Duration

	size    uint64 `stats:"atomic"`
	misses  uint64 `stats:"atomic"`
	lookups uint64 `stats:"atomic"`
	expires uint64 `stats:"atomic"`

	reporter netstats.Reporter
}

const defaultTTL = 2 * time.Minute

// RouteCache is the interface to a cache that stores routes for a given (source, destination, net ns) tuple
type RouteCache interface {
	Get(source, dest util.Address, netns uint32) (Route, bool)
	GetStats() map[string]interface{}
	Close()
}

// Router is an interface to get a route for a (source, destination, net ns) tuple
type Router interface {
	Route(source, dest util.Address, netns uint32) (Route, bool)
	GetStats() map[string]interface{}
	Close()
}

// NewRouteCache creates a new RouteCache
func NewRouteCache(size int, router Router) RouteCache {
	return newRouteCache(size, router, defaultTTL)
}

// newRouteCache is a private method used primarily for testing
func newRouteCache(size int, router Router, ttl time.Duration) *routeCache {
	if router == nil {
		return nil
	}

	rc := &routeCache{
		cache:  lru.New(size),
		router: router,
		ttl:    ttl,
	}

	var err error
	rc.reporter, err = netstats.NewReporter(rc)
	if err != nil {
		panic("could not create stats reporter for route cache")
	}

	return rc
}

func (c *routeCache) Close() {
	c.router.Close()
}

func (c *routeCache) Get(source, dest util.Address, netns uint32) (Route, bool) {
	c.Lock()
	defer c.Unlock()

	atomic.AddUint64(&c.lookups, 1)
	k := newRouteKey(source, dest, netns)
	if entry, ok := c.cache.Get(k); ok {
		if time.Now().Unix() < entry.(*routeTTL).eta {
			return entry.(*routeTTL).entry, ok
		}

		atomic.AddUint64(&c.expires, 1)
		c.cache.Remove(k)
		atomic.AddUint64(&c.size, ^uint64(0))
	} else {
		atomic.AddUint64(&c.misses, 1)
	}

	if r, ok := c.router.Route(source, dest, netns); ok {
		entry := &routeTTL{
			eta:   time.Now().Add(c.ttl).Unix(),
			entry: r,
		}

		c.cache.Add(k, entry)
		atomic.AddUint64(&c.size, 1)
		return r, true
	}

	return Route{}, false
}

func (c *routeCache) GetStats() map[string]interface{} {
	stats := c.reporter.Report()
	stats["router"] = c.router.GetStats()
	return stats
}

func newRouteKey(source, dest util.Address, netns uint32) routeKey {
	k := routeKey{netns: netns, source: source, dest: dest}

	switch dest.Len() {
	case 4:
		k.connFamily = AFINET
	case 16:
		k.connFamily = AFINET6
	}
	return k
}

type ifkey struct {
	ip    util.Address
	netns uint32
}

type ifEntry struct {
	index    int
	loopback bool
}

type netlinkRouter struct {
	rootNs  uint32
	ioctlFD int
	ifcache *lru.Cache

	netlinkLookups uint64 `stats:"atomic"`
	netlinkErrors  uint64 `stats:"atomic"`
	netlinkMisses  uint64 `stats:"atomic"`

	ifCacheLookups uint64 `stats:"atomic"`
	ifCacheMisses  uint64 `stats:"atomic"`
	ifCacheSize    uint64 `stats:"atomic"`
	ifCacheErrors  uint64 `stats:"atomic"`

	reporter netstats.Reporter
}

// NewNetlinkRouter create a Router that queries routes via netlink
func NewNetlinkRouter(procRoot string) (Router, error) {
	rootNs, err := util.GetNetNsInoFromPid(procRoot, 1)
	if err != nil {
		return nil, fmt.Errorf("netlink gw cache backing: could not get root net ns: %w", err)
	}

	var fd int
	err = util.WithRootNS(procRoot, func() (sockErr error) {
		fd, sockErr = unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		return
	})
	if err != nil {
		return nil, err
	}

	nr := &netlinkRouter{
		rootNs:  rootNs,
		ioctlFD: fd,
		// ifcache should ideally fit all interfaces on a given node
		ifcache: lru.New(128),
	}

	nr.reporter, err = netstats.NewReporter(nr)
	if err != nil {
		return nil, fmt.Errorf("error creating stats reporter: %w", err)
	}

	return nr, nil
}

func (n *netlinkRouter) Close() {
	unix.Close(n.ioctlFD)
}

func (n *netlinkRouter) GetStats() map[string]interface{} {
	return n.reporter.Report()
}

func (n *netlinkRouter) Route(source, dest util.Address, netns uint32) (Route, bool) {
	var iifIndex int

	srcBuf := util.IPBufferPool.Get().(*[]byte)
	dstBuf := util.IPBufferPool.Get().(*[]byte)
	defer func() {
		util.IPBufferPool.Put(srcBuf)
		util.IPBufferPool.Put(dstBuf)
	}()

	srcIP := util.NetIPFromAddress(source, *srcBuf)
	if n.rootNs != netns {
		// if its a non-root ns, we're dealing with traffic from
		// a container most likely, and so need to find out
		// which interface is associated with the ns

		// get input interface for src ip
		iif := n.getInterface(source, srcIP, netns)
		if iif == nil || iif.index == 0 {
			return Route{}, false
		}

		if !iif.loopback {
			iifIndex = iif.index
		}
	}

	atomic.AddUint64(&n.netlinkLookups, 1)
	dstIP := util.NetIPFromAddress(dest, *dstBuf)
	routes, err := netlink.RouteGetWithOptions(
		dstIP,
		&netlink.RouteGetOptions{
			SrcAddr:  srcIP,
			IifIndex: iifIndex,
		})

	if err != nil {
		atomic.AddUint64(&n.netlinkErrors, 1)
	}
	if len(routes) != 1 {
		atomic.AddUint64(&n.netlinkMisses, 1)
	}
	if err != nil || len(routes) != 1 {
		log.Tracef("could not get route for src=%s dest=%s err=%s routes=%+v", source, dest, err, routes)
		return Route{}, false
	}

	r := routes[0]
	log.Tracef("route for src=%s dst=%s: scope=%s gw=%+v if=%d", source, dest, r.Scope, r.Gw, r.LinkIndex)
	return Route{
		Gateway: util.AddressFromNetIP(r.Gw),
		IfIndex: r.LinkIndex,
	}, true
}

func (n *netlinkRouter) getInterface(srcAddress util.Address, srcIP net.IP, netns uint32) *ifEntry {
	atomic.AddUint64(&n.ifCacheLookups, 1)

	key := ifkey{ip: srcAddress, netns: netns}
	if entry, ok := n.ifcache.Get(key); ok {
		return entry.(*ifEntry)
	}
	atomic.AddUint64(&n.ifCacheMisses, 1)

	atomic.AddUint64(&n.netlinkLookups, 1)
	routes, err := netlink.RouteGet(srcIP)
	if err != nil {
		atomic.AddUint64(&n.netlinkErrors, 1)
		return nil
	}
	if len(routes) != 1 {
		atomic.AddUint64(&n.netlinkMisses, 1)
		return nil
	}

	ifr, err := unix.NewIfreq("")
	if err != nil {
		atomic.AddUint64(&n.ifCacheErrors, 1)
		return nil
	}

	ifr.SetUint32(uint32(routes[0].LinkIndex))
	// first get the name of the interface. this is
	// necessary to make the subsequent request to
	// get the link flags
	if err = unix.IoctlIfreq(n.ioctlFD, unix.SIOCGIFNAME, ifr); err != nil {
		atomic.AddUint64(&n.ifCacheErrors, 1)
		log.Tracef("error getting interface name for link index %d: %s", routes[0].LinkIndex, err)
		return nil
	}
	if err = unix.IoctlIfreq(n.ioctlFD, unix.SIOCGIFFLAGS, ifr); err != nil {
		atomic.AddUint64(&n.ifCacheErrors, 1)
		log.Tracef("error getting interface flags for link index %d: %s", routes[0].LinkIndex, err)
		return nil
	}

	iff := &ifEntry{index: routes[0].LinkIndex, loopback: (ifr.Uint16() & unix.IFF_LOOPBACK) != 0}
	log.Tracef("adding interface entry, key=%+v, entry=%v", key, *iff)
	n.ifcache.Add(key, iff)
	atomic.AddUint64(&n.ifCacheSize, 1)
	return iff
}
