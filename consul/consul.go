package coredns_consul

import (
	"context"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// Consul is the implementation of the "consul" CoreDNS plugin.
//
// Consul instances are safe to use concurrently from multiple goroutines.
//
// Consul instances must not be copied after the first time they are used,
// referencing them by pointer should be preferred to passing them by value.
type Consul struct {
	Next plugin.Handler // Next handler in the list of plugins.

	// Addr is the address of the consul agent used by this plugin, it must be
	// be in the scheme://host:port format.
	Addr string

	// Maximum age of cached service entries.
	TTL time.Duration

	// Maximum number of inflight requests per target.
	MaxRequests int

	// Configuration of the cache prefetcher.
	PrefetchAmount     int
	PrefetchPercentage int
	PrefetchDuration   time.Duration

	// HTTP transport used to send requests to consul.
	Transport http.RoundTripper

	once  sync.Once
	cache *cache
}

const (
	defaultAddr               = "http://localhost:8500"
	defaultTTL                = 1 * time.Minute
	defaultMaxRequests        = 8192
	defaultPrefetchAmount     = 2
	defaultPrefetchPercentage = 10
	defaultPrefetchDuration   = 1 * time.Minute
)

// Consul constructs a new instance of a consul plugin.
func New() *Consul {
	return &Consul{
		Addr:               defaultAddr,
		TTL:                defaultTTL,
		MaxRequests:        defaultMaxRequests,
		PrefetchAmount:     defaultPrefetchAmount,
		PrefetchPercentage: defaultPrefetchPercentage,
		PrefetchDuration:   defaultPrefetchDuration,
	}
}

// Name of the plugin, returns "consul".
func (*Consul) Name() string { return "consul" }

// ServeDNS satisfies the plugin.Handler interface.
func (c *Consul) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	rcode, answer, extra, err := c.serveDNS(ctx, state)

	a := &dns.Msg{}
	a.SetReply(r)
	a.Rcode = rcode
	a.Compress = true
	a.Authoritative = true

	if answer != nil {
		a.Answer = append(a.Answer, answer)
	}

	if extra != nil {
		a.Extra = append(a.Extra, extra)
	}

	state.SizeAndDo(a)
	a, _ = state.Scrub(a)
	w.WriteMsg(a)
	return rcode, err
}

func (c *Consul) serveDNS(ctx context.Context, state request.Request) (rcode int, answer dns.RR, extra dns.RR, err error) {
	c.once.Do(c.init)

	qname := state.Name()
	qtype := state.QType()

	name, tag, typ, dc, domain := splitName(qname)
	if len(name) == 0 {
		rcode = dns.RcodeNameError
		return
	}
	if domain != "consul" {
		rcode = dns.RcodeRefused
		return
	}
	if typ != "service" {
		rcode = dns.RcodeNotImplemented
		return
	}

	key := key{name: name, tag: tag, dc: dc, qtype: qtype}
	switch key.qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypeANY:
	case dns.TypeSRV:
		key.qtype = dns.TypeANY
	default:
		rcode = dns.RcodeNotImplemented
		return
	}

	res := make(chan serviceResponse, 1)
	req := serviceRequest{key: key, res: res}

	select {
	case c.cache.reqs <- req:
	case <-ctx.Done():
		rcode, err = dns.RcodeServerFailure, ctx.Err()
		return
	}

	var found serviceResponse
	select {
	case found = <-res:
	case <-ctx.Done():
		rcode, err = dns.RcodeServerFailure, ctx.Err()
		return
	}
	if found.err != nil {
		rcode, err = dns.RcodeServerFailure, found.err
		return
	}
	if found.srv.addr == nil {
		rcode = dns.RcodeNameError
		return
	}

	switch qtype {
	case dns.TypeA:
		answer = found.A(qname)
	case dns.TypeAAAA:
		answer = found.AAAA(qname)
	case dns.TypeANY:
		answer = found.ANY(qname)
	case dns.TypeSRV:
		srv := found.SRV(qname)
		answer = srv
		extra = found.ANY(srv.Target)
	}
	return
}

func (c *Consul) init() {
	log.Printf("[INFO] consul %s { ttl %s; maxreq %d; prefetch %d %s %d%% }",
		c.Addr, c.TTL, c.MaxRequests, c.PrefetchAmount, c.PrefetchDuration, c.PrefetchPercentage)

	reqs := make(chan serviceRequest, c.MaxRequests)

	cache := &cache{
		reqs:               reqs,
		addr:               c.Addr,
		ttl:                c.TTL,
		maxRequests:        c.MaxRequests,
		prefetchAmount:     c.PrefetchAmount,
		prefetchPercentage: c.PrefetchPercentage,
		prefetchDuration:   c.PrefetchDuration,
		transport:          c.Transport,
	}

	if cache.transport == nil {
		cache.transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     2 * c.TTL,
		}
	}

	go cache.serve(reqs)

	c.cache = cache
	runtime.SetFinalizer(c, func(c *Consul) { c.cache.close() })
}

func splitName(s string) (name, tag, typ, dc, domain string) {
	s = strings.TrimSuffix(s, ".")
	if strings.HasPrefix(s, "_") {
		return splitNameRFC2782(s)
	}
	return splitNameDefault(s)
}

func splitNameDefault(s string) (name, tag, typ, dc, domain string) {
	for _, sep := range []string{".service.", ".query."} {
		if i := strings.Index(s, sep); i >= 0 {
			name, tag = splitLast(s[:i])
			domain, dc = splitLast(s[i+len(sep):])
			typ = sep
			typ = strings.TrimPrefix(typ, ".")
			typ = strings.TrimSuffix(typ, ".")
			break
		}
	}
	return
}

func splitNameRFC2782(s string) (name, tag, typ, dc, domain string) {
	name, s = split(s)
	tag, s = split(s)

	if domain, s = split(s); domain == "service" {
		if domain, s = split(s); len(s) != 0 {
			dc = domain
			if domain, s = split(s); len(s) != 0 {
				name = ""
				return
			}
		}
	}

	if tag == "_tcp" {
		tag = ""
	} else if !strings.HasPrefix(tag, "_") {
		name = ""
		return
	}

	name = strings.TrimPrefix(name, "_")
	tag = strings.TrimPrefix(tag, "_")
	typ = "service"
	return
}

func split(s string) (token, remain string) {
	if i := strings.IndexByte(s, '.'); i < 0 {
		token = s
	} else {
		token, remain = s[:i], s[i+1:]
	}
	return
}

func splitLast(s string) (token, remain string) {
	if i := strings.LastIndexByte(s, '.'); i < 0 {
		token = s
	} else {
		token, remain = s[i+1:], s[:i]
	}
	return
}