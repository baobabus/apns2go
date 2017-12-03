// Copyright 2017 Aleksey Blinov. All rights reserved.

package apns2

import (
	"fmt"
	"time"
)

type ProcRate float64

// ProcCfg is a set of parameters that govern request processing flow
// including automatic scaling of the processing pipeline.
type ProcCfg struct {

	// MaxRetries is the maximum number of times a failed notification push
	// should be reattempted. This only applies to "retriable" failures.
	MaxRetries uint32

	// RetryEval is the function that is called when a push attempt fails
	// and retry eligibility needs to be determined.
	RetryEval func(*Response, error) bool

	// MinConns is minimum number of concurrent connections to APN servers
	// that should be kept open.
	MinConns   uint32

	// MaxConns is maximum allowed number of concurrent connections
	// to APN service.
	MaxConns   uint32

	// MaxRate is the throughput cap specified in notifications per second.
	// It is not strictly enforced as would be the case with a true rate
	// limiter. Instead it only prevents additional scaling from taking place
	// once the specified rate is reached.
	MaxRate    ProcRate

	// MaxBandwidth is the throughput cap specified in bits per second.
	// It is not strictly enforced as would be the case with a true rate
	// limiter. Instead it only prevents additional scaling from taking place
	// once the specified rate is reached.
	MaxBandwidth ProcRate

	// Scale specifies the manner of scaling up and winding down.
	// Two scaling modes come prefefined: Incremental and Exponential.
	// See below for more detail.
	Scale      Scale

	// MinSustain is the minimum duration of time over which the processing
	// has to experience blocking before a scale-up attemp is made. It is also
	// the minimum amount of time over which non-blocking processing has to
	// take place before a wind-down attemp is made.
	MinSustain time.Duration

	// PollInterval is the time between performance metrics sampling attempts.
	PollInterval time.Duration

	// SettlePeriod is the amount of time given to the processing for it to
	// settle down at the new rate after successful scaling up or
	// winding down attempt. Sustained performance analysis is ignored during
	// this time and no new scaling attempt is made.
	SettlePeriod time.Duration

	// AllowHTTP2Incursion controls whether it is OK to perform reflection-based
	// probing of HTTP/2 layer. When enabled, scaler may access certain private
	// properties in x/net/http2 package if needed for more precise performance
	// analysis.
	AllowHTTP2Incursion bool

	// UsePreciseHTTP2Metrics, if set to true, instructs the scaler to query
	// HTTP/2 layer parameters on every call that requires the data.
	// Set this to false if you wish to eliminate any additional overhead that
	// this may introduce.
	UsePreciseHTTP2Metrics bool

	// HTTP2MetricsRefreshPeriod, if set to a positive value, controls
	// the frequency of "imprecise" metrics updates. Under this approach any
	// relevant fields that are private to x/net/http2 packaged are only
	// queried periodically.
	// This reduces the overhead of any required reflection calls, but it also
	// introduces the risk of potentially relying on some stale metrics.
	// In most realistic situations, however, this can be easily tolerated
	// given frequent enough refresh period.
	//
	// HTTP2MetricsRefreshPeriod value is ignored and periodic updates
	// are turned off if UsePreciseHTTP2Metrics is set to true.
	// Setting HTTP2MetricsRefreshPeriod to 0 or negative value disables
	// metrics refresh even if UsePreciseMetrics is false.
	HTTP2MetricsRefreshPeriod time.Duration

}

// MinBlockingProcConfig is a configuration with absolute mimimal processing
// settings. It only allows a single connection to APN service with no scaling.
// HTTP/2 layer metrics refresh is set to 500ms to allow proper handling
// of HTTP/2 streams concurrency without introducing any noticeable overhead.
var MinBlockingProcConfig =  ProcCfg {
	MinConns:                  1,
	MaxConns:                  1,
	Scale:                     Constant,
	AllowHTTP2Incursion:       true,
	HTTP2MetricsRefreshPeriod: 500 * time.Millisecond,
}

// minSustainPollPeriods returns the number of PollInterval periods per
// MinSustain time interval. If PollInterval is not a whole divisor of
// MinSustain, the result is rounded up.
// If either PollInterval or MinSustain is not a valid time interval,
// max uint32 is returned.
func (c *ProcCfg) minSustainPollPeriods() uint32 {
	if c.MinSustain <= 0 || c.PollInterval <= 0 {
		return ^uint32(0)
	}
	res := c.MinSustain / c.PollInterval
	if c.MinSustain % c.PollInterval > 0 {
		res++
	}
	return uint32(res)
}

type governor struct {
	id string
	c          *Client
	ctl        <-chan struct{}
	done       chan<- struct{}

	cfg       ProcCfg

	// minimun number of continuous sampling periods of performance
	// evaluation need to have an effect on scaling decision
	minSust    uint32

	// should following launch attemtps be blocked by pending ones
	nonBlocking bool

	// counters of continuous periods with waits and no waits
	// on inbound and oubound channels
	inCtr  waitCounter
	outCtr waitCounter

	retry chan *Request

	// active streamers and pending launchers
	streamers map[*streamer]chan struct{}
	launchers map[*launcher]chan struct{}
	nextWId   uint

	// "callback" channels streamers and launchers
	// to annouce their completion
	wExits   chan *streamer
	lExits   chan *launcher

	// time of last up- or down-scaling
	lastScale time.Time

	isClosing bool
}

type waitCounter struct {
	waits   uint32
	noWaits uint32
}

func (c *waitCounter) acc(val uint32) {
	if val > 0 {
		c.waits++
		c.noWaits = 0
	} else {
		c.waits = 0
		c.noWaits++
	}
}

// Must be called exactly once
func (g *governor) run() {
	logInfo(g.id, "Starting.")
	g.wExits = make(chan *streamer)
	g.lExits = make(chan *launcher)
	g.streamers = make(map[*streamer]chan struct{})
	g.launchers = make(map[*launcher]chan struct{})
	go g.runRetryForwarder()
	// Launch first MinConns streamers
	g.tryScaleUp()
	var tkrChan <-chan time.Time
	if g.cfg.PollInterval > 0 {
		tkr := time.NewTicker(g.cfg.PollInterval)
		defer tkr.Stop()
		tkrChan = tkr.C
	}
	logInfo(g.id, "Running.")
	for done := false; !done; {
		select {
		case l := <-g.lExits:
			// launcher finished
			delete(g.launchers, l)
			if w := l.worker; w != nil {
				g.streamers[w] = w.ctl
			} else {
				if l.err != nil {
					logWarn(g.id, "Error starting streamer: %v", l.err)
				}
			}
			// TODO Handle failed launches
		case w := <-g.wExits:
			// worker finished
			if w.inClosed && !g.isClosing {
				// Soft stop: Client closed main channel. We are closing, too.
				logInfo(g.id, "Stopping.")
				g.isClosing = true
			}
			delete(g.streamers, w)
			if w.didQuit {
				// This needs to be on exponential back-off
				g.launchStreamer()
			}
		case <-tkrChan:
			if g.isClosing {
				break
			}
			s := g.updateCountersAndEvalScaling()
			if s > 0 {
				g.tryScaleUp()
			} else if s < 0 {
				g.tryWindDown()
			}
		case <-g.ctl:
			// Hard stop command
			logInfo(g.id, "Terminating.")
			done = true
		}
		if !done && g.isClosing {
			done = len(g.streamers) == 0 && len(g.launchers) == 0
		}
	}
	// signal launchers and streamers
	logInfo(g.id, "Terminating launchers and streamers.")
	for i, _ := range g.launchers {
		close(i.ctl)
	}
	for i, _ := range g.streamers {
		close(i.ctl)
	}
	// TODO Signal forwarder to stop
	logInfo(g.id, "Stopped.")
	// Signal parent
	close(g.done)
}

func (g *governor) updateCountersAndEvalScaling() int {
	// It is ok for the calls to Fold to not be fully synchronized.
	// We are only roughly estimating the disparity.
	ics, _ := g.c.waitCtr.Fold()
	var ocs uint32
	for w, _ := range g.streamers {
		oc, _ := w.waitCtr.Fold()
		ocs += oc
	}
	g.inCtr.acc(ics)
	g.outCtr.acc(ocs)
	if g.inCtr.waits >= g.minSust && g.outCtr.noWaits >= g.minSust {
		return 1
	} else if g.inCtr.noWaits >= g.minSust {
		return -1
	}
	return 0
}

const (
	forScaleUp  = true
	forWindDown = false
)

func (g *governor) tryScaleUp() {
	delta := g.allowedScaleDelta(forScaleUp)
	logTrace(0, g.id, "tryScaleUp delta = %d", delta)
	if delta <= 0 {
		return
	}
	for i := 0; i < delta; i++ {
		g.launchStreamer()
	}
}

func (g *governor) tryWindDown() {
	// TODO Implement winding down
}

func (g *governor) launchStreamer() {
	wid := fmt.Sprintf(g.id + "-Streamer-%d", g.nextWId)
	l := &launcher{gov: g, id: wid, done: g.lExits, ctl: make(chan struct{})}
	g.nextWId++
	g.launchers[l] = l.ctl
	go l.launch()
}

func (g *governor) allowedScaleDelta(forScaleUp bool) int {
	if g.isClosing {
		return 0
	}
	now := time.Now()
	if g.lastScale.Add(g.cfg.SettlePeriod).After(now) {
		return 0
	}
	if !g.nonBlocking && len(g.launchers) > 0 {
		return 0
	}
	prov := uint32(len(g.streamers) + len(g.launchers))
	req := uint32(0)
	if forScaleUp {
		if prov >= g.cfg.MaxConns {
			return 0
		}
		req = g.cfg.Scale.Apply(prov)
	} else {
		if prov <= g.cfg.MinConns {
			return 0
		}
		req = g.cfg.Scale.ApplyInverse(prov)
	}
	if req < g.cfg.MinConns {
		req = g.cfg.MinConns
	}
	if req > g.cfg.MaxConns {
		req = g.cfg.MaxConns
	}
	return int(req) - int(prov)
}

type launcher struct {
	gov    *governor
	id     string
	done   chan<- *launcher
	ctl    chan struct{}
	err    error
	worker *streamer
}

func (l *launcher) launch() {
	w := &streamer{
		id:   l.id,
		c:    l.gov.c,
		gov:  l.gov,
		in:   l.gov.c.out,
		out:  l.gov.c.Callback,
		warmStart: true,
		ctl:  make(chan struct{}),
		done: l.gov.wExits,
	}
	if l.err = w.start(nil); l.err == nil {
		l.worker = w
	}
	// read from ctl prevents blocking on done if the governor
	// was commanded to terminate in the meantime
	select {
	case l.done<- l:
	case <-l.ctl:
	}
}

// TODO Rework forwarder and streamers so that inbound channel can be closed
// by the client to indicate end of input, while allowing any retry requests
// to finish.
func (g *governor) runRetryForwarder() {
	if g.cfg.MaxRetries == 0 {
		return
	}
	// Retry requests will be re-queued with the Client. We need to ensure
	// that any blocking on the Client inbound channel is dealt with in a way
	// that doesn't block our streamers.
	// Rather than spinning goroutines for every retry send, we buffer
	// the sends. 100 buffered forwarders with buffers of 500 requests each
	// is more efficient than 50000 individual sender goroutines.
	var buf chan *Request
	bufSize := 500
	cnt := 0
	// slight buffering on the inbound channel to improve performance
	g.retry = make(chan *Request, 100)
	logInfo(g.id + "-RetryForwarder", "Running.")
	for done := false; !done; {
		select {
		case req := <-g.retry:
			if buf == nil || cnt >= bufSize {
				if buf != nil {
					// signal bufferedForwarder to return
					close(buf)
				}
				buf = make(chan *Request, bufSize)
				go bufferedForwarder(buf, g.c, g.ctl)
				cnt = 0
			}
			buf <- req
		case <-g.ctl:
			done = true
		}
	}
	logInfo(g.id + "-RetryForwarder", "Stopped.")
}

func bufferedForwarder(in <-chan *Request, client *Client, ctl <-chan struct{}) {
	for done := false; !done; {
		select {
		case req, ok := <-in:
			if !ok {
				done = true
				break
			}
			select {
			case client.retry<- req:
			case <-ctl:
				done = true
			}
		case <-ctl:
			done = true
		}
	}
}
