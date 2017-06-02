// Bounded is Consistent hashing with bounded loads.
// It acheives that by adding a capacity counter on every host,
// and when a host gets picked it, checks its capacity to see if it's below
// the Average Load per Host.
//
// All opertaions in bounded are concurrency-safe.
//
// Average Load Per Host is defined as follows:
// (totalLoad/number_of_hosts)*imbalance_constant
// totalLoad = sum of all hosts load
// load = the number of active requests
// imbalance_constant = is the imbalance constant, which is 1.25 in our case
// it bounds the load imabalnce to be at most 25% more than (totalLoad/number_of_hosts)
package bounded

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/lafikl/liblb"
	"github.com/prometheus/client_golang/prometheus"

	"stathat.com/c/consistent"
)

var ErrAllOverloaded = errors.New("all hosts are overloaded")
var Err = errors.New("all hosts are overloaded")

type bhost struct {
	load   uint64
	weight int
}

type Bounded struct {
	ch        *consistent.Consistent
	loads     map[string]*bhost
	totalLoad uint64

	enableMetrics bool
	servedReqs    *prometheus.CounterVec
	errCounter    *prometheus.CounterVec

	sync.RWMutex
}

func New(hosts ...string) *Bounded {
	c := &Bounded{
		ch:    consistent.New(),
		loads: map[string]*bhost{},
	}
	for _, h := range hosts {
		c.Add(h)
	}
	return c
}

// Registers "liblb_consistent_bounded_requests_total" and
// "liblb_consistent_bounded_errors_total" in prometheus.
func (c *Bounded) EnableMetrics() error {
	c.Lock()
	defer c.Unlock()

	sreq := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "liblb_consistent_bounded_requests_total",
		Help: "Number of requests served by Consistent Bounded",
	}, []string{"host"})

	err := prometheus.Register(sreq)
	if err != nil {
		return err
	}

	errCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "liblb_consistent_bounded_errors_total",
		Help: "Number of times Bounded failed",
	}, []string{"type"})

	err = prometheus.Register(errCounter)
	if err != nil {
		return err
	}

	c.servedReqs = sreq
	c.errCounter = errCounter
	c.enableMetrics = true

	return nil
}

func (b *Bounded) Add(host string) {
	b.AddWeight(host, 1)
}

// Weight increases the max load of a host in the following way:
// MaxLoad(host)*weight
func (b *Bounded) AddWeight(host string, weight int) {
	b.Lock()
	defer b.Unlock()

	if _, ok := b.loads[host]; ok {
		return
	}

	b.loads[host] = &bhost{load: 0, weight: weight}
	b.ch.Add(host)
}

func (b *Bounded) Remove(host string) {
	b.Lock()
	defer b.Unlock()

	load, ok := b.loads[host]
	if !ok {
		return
	}

	b.totalLoad -= load.load

	delete(b.loads, host)
	b.ch.Remove(host)
}

// err can be either liblb.ErrNoHost if there's no added hosts.
// Or ErrAllOverloaded  if all hosts are overloaded, which shoould never happen.
// But it's added to make sure that if it happens because of
// a bug in the implementation, the application can handle it.
func (b *Bounded) Balance(key string) (host string, err error) {
	b.Lock()
	defer b.Unlock()

	if len(b.ch.Members()) == 0 {
		return "", liblb.ErrNoHost
	}

	host, err = b.get("", key, 10)
	if err != nil {
		if b.enableMetrics {
			b.updateErrCount(err)
		}
		return
	}

	if b.enableMetrics {
		b.servedReqs.WithLabelValues(host).Inc()
	}

	return
}

func (b *Bounded) updateErrCount(err error) {
	typ := "empty"
	if err == ErrAllOverloaded {
		typ = "overloaded"
	}
	b.errCounter.WithLabelValues(typ).Inc()
}

func (b *Bounded) get(firstKey, currentKey string, size int) (string, error) {
	hosts, err := b.ch.GetN(currentKey, size)
	if err != nil {
		return "", err
	}

	for _, host := range hosts {
		if host == firstKey {
			return "", ErrAllOverloaded
		}
		if b.loadOK(host) {
			b.loads[host].load++
			b.totalLoad++
			return host, nil
		}
	}
	if len(firstKey) == 0 {
		firstKey = hosts[0]
	}
	currentKey = hosts[len(hosts)-1]
	// return b.get(firstKey, currentKey, size*3/2)
	return b.get(firstKey, currentKey, size)
}

func (b *Bounded) Done(host string) {
	b.Lock()
	defer b.Unlock()

	bhost, ok := b.loads[host]
	if !ok {
		return
	}
	bhost.load--
	b.totalLoad--
}

func (b *Bounded) Loads() map[string]uint64 {
	loads := map[string]uint64{}
	for k, bhost := range b.loads {
		loads[k] = bhost.load
	}
	return loads
}

func (b *Bounded) Weights() map[string]uint64 {
	weights := map[string]uint64{}
	for k, bhost := range b.loads {
		weights[k] = uint64(bhost.weight)
	}
	return weights
}

func (b *Bounded) loadOK(host string) bool {
	// calcs load
	if b.totalLoad == 0 {
		b.totalLoad = 1
	}
	var avgLoadPerNode float64
	avgLoadPerNode = float64(b.totalLoad / uint64(len(b.loads)))
	if avgLoadPerNode == 0 {
		avgLoadPerNode = 1
	}
	avgLoadPerNode = math.Ceil(avgLoadPerNode * 1.25)

	bhost, ok := b.loads[host]
	if !ok {
		panic(fmt.Sprintf("given host(%s) not in loadsMap", host))
	}

	if float64(bhost.load)+1 <= (avgLoadPerNode * float64(bhost.weight)) {
		return true
	}

	return false
}

// Average Load Per Host is:
// (totalLoad/number_of_hosts)*imbalance_constant
// totalLoad = sum of all hosts load
// load = the number of active requests
// imbalance_constant = is the imbalance constant, which is 1.25 in our case
// it bounds the load imabalnce to be at most 25% more than (totalLoad/number_of_hosts)
func (b *Bounded) AvgLoad() uint64 {
	b.Lock()
	defer b.Unlock()

	var avgLoadPerNode float64
	avgLoadPerNode = float64(b.totalLoad / uint64(len(b.loads)))
	if avgLoadPerNode == 0 {
		avgLoadPerNode = 1
	}
	avgLoadPerNode = math.Ceil(avgLoadPerNode * 1.25)
	return uint64(avgLoadPerNode)
}

// Max load of a host is (Average Load Per Host*Host Weight)
func (b *Bounded) MaxLoad(host string) uint64 {
	avg := b.AvgLoad()

	b.Lock()
	defer b.Unlock()
	bh, ok := b.loads[host]
	if !ok {
		return 0
	}
	return avg * uint64(bh.weight)
}
