package main

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	beeline "github.com/honeycombio/beeline-go"
	"github.com/honeycombio/beeline-go/propagation"
	"github.com/honeycombio/beeline-go/trace"
	"github.com/honeycombio/beeline-go/wrappers/config"
	"github.com/honeycombio/beeline-go/wrappers/hnynethttp"
	"github.com/honeycombio/leakybucket"
)

// rate limiting proxy is a forwarding proxy that has a built in rate limit. In
// the style of a tarpit, it waits a random amount of time before returning when
// rate limiting connections. The forwarding target and rate limits are hard
// coded.  Based on the client IP address, it allows through bursts of up to 50
// and sustained requests of 8 per second for all GET requests. It allows a
// burst of 10 and sustained 1/sec for all other HTTP methods (eg POST, PUT,
// etc.).

const (
	// defaultDownstreamTarget is who this proxy fronts
	defaultDownstreamTarget = "http://localhost:7000"

	// the default limits for GET and all other requests
	getBurstLimit        = 50
	getThroughputLimit   = 8
	otherBurstLimit      = 10
	otherThroughputLimit = 1

	// how long should we hold on to rate limited request connections (in ms)?
	waitBaseTime = 100.0
	waitRange    = 500.0
	waitStdDev   = 100.0
)

var downstreamTarget = "http://localhost:7000"

type app struct {
	client      *http.Client
	rateLimiter map[string]*leakybucket.Bucket
	sync.Mutex
}

func main() {

	wk := os.Getenv("HONEYCOMB_APIKEY")
	var useStdout bool
	if wk == "" {
		useStdout = true
	}

	// optionally get the listen port and downstream target from the environment
	port := os.Getenv("HONEYCOMB_LISTEN_PORT")
	if port == "" {
		port = "8080"
	}
	dsTarget := os.Getenv("HONEYCOMB_DOWNSTREAM_TARGET")
	if dsTarget == "" {
		dsTarget = defaultDownstreamTarget
	}
	downstreamTarget = dsTarget

	// Initialize beeline. The only required field is WriteKey.
	beeline.Init(beeline.Config{
		WriteKey:    wk,
		Dataset:     "beeline-example",
		ServiceName: "rlp-" + port,

		// In no writekey is configured, send the event to STDOUT instead of Honeycomb.
		STDOUT: useStdout,
	})

	defaultTransport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 6 * time.Second,
	}
	wrapRTConfig := config.HTTPOutgoingConfig{
		HTTPPropagationHook: func(req *http.Request, prop *propagation.PropagationContext) map[string]string {
			if existingHeader := req.Header.Get("X-Amzn-Trace-Id"); existingHeader != "" {
				req.Header.Del("X-Amzn-Trace-Id")
			}
			return map[string]string{
				"X-Amzn-Trace-Id": propagation.MarshalAmazonTraceContext(prop),
			}
		},
	}
	// create a custom client that has sane timeouts and records outbound events
	client := &http.Client{
		Timeout:   time.Second * 10,
		Transport: hnynethttp.WrapRoundTripperWithConfig(defaultTransport, wrapRTConfig),
	}

	a := &app{
		client:      client,
		rateLimiter: make(map[string]*leakybucket.Bucket),
	}

	wrapHandlerConfig := config.HTTPIncomingConfig{
		HTTPParserHook: func(r *http.Request) *propagation.PropagationContext {
			propHeader := r.Header.Get("X-Amzn-Trace-Id")
			prop, _ := propagation.UnmarshalAmazonTraceContext(propHeader)
			return prop
		},
	}
	http.Handle("/", hnynethttp.WrapHandlerWithConfig(a, wrapHandlerConfig))
	log.Fatal(http.ListenAndServe("localhost:"+port, nil))
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.proxy(w, r)
	return
}
func (a *app) proxy(w http.ResponseWriter, req *http.Request) {
	curSpan := trace.GetSpanFromContext(req.Context())
	ctx, span := curSpan.CreateAsyncChild(req.Context())
	span.AddField("name", "extra span")
	defer span.Send() // whoops we forgot
	var rateKey string
	forwarded := req.Header.Get("X-Forwarded-For")
	beeline.AddField(ctx, "forwarded_for_incoming", forwarded)
	if forwarded == "" {
		rateKey = strings.Split(req.RemoteAddr, ":")[0]
	} else {
		rateKey = forwarded
	}

	// check rate limits
	beeline.AddField(ctx, "rate_limit_key", rateKey)
	hitCapacity := a.shouldRateLimit(ctx, req.Method, rateKey)
	if hitCapacity != nil {
		beeline.AddField(ctx, "error", "rate limit exceeded")
		ctx, span := beeline.StartSpan(ctx, "wait")
		defer span.Send()
		sleepTime := math.Abs(waitBaseTime + (rand.NormFloat64()*waitStdDev + waitRange))
		beeline.AddField(ctx, "wait_time", sleepTime)
		// sleep a random amount in the range (waitBaseTime to waitBaseTime+waitRange)
		// aka 100-600ms
		time.Sleep(time.Duration(sleepTime) * time.Millisecond)

		// ok, go ahead and reply
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limit exceeded; please wait 1sec and try again"}`)
		return
	}
	// ok we're allowed to proceed, let's copy the request over to a new one and
	// dispatch it downstream
	defer req.Body.Close()
	reqBod, _ := ioutil.ReadAll(req.Body)
	buf := bytes.NewBuffer(reqBod)
	downstreamReq, err := http.NewRequest(req.Method, downstreamTarget+req.URL.String(), buf)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"failed to create downstream request"}`)
		beeline.AddField(ctx, "error", err)
		beeline.AddField(ctx, "error_detail", "failed to create downstream request")
		return
	}
	// add context to propagate the beeline trace
	downstreamReq = downstreamReq.WithContext(ctx)
	// copy over headers from upstream to the downstream service
	for header, vals := range req.Header {
		downstreamReq.Header.Set(header, strings.Join(vals, ","))
	}
	if forwarded != "" {
		downstreamReq.Header.Set("X-Forwarded-For", forwarded+", "+req.RemoteAddr)
	} else {
		downstreamReq.Header.Set("X-Forwarded-For", req.RemoteAddr)
	}
	beeline.AddField(ctx, "forwarded_for_outgoing", downstreamReq.Header.Get("X-Forwarded-For"))
	// call the downstream service
	resp, err := a.client.Do(downstreamReq)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"downstream target unavailable"}`)
		beeline.AddField(ctx, "error", err)
		beeline.AddField(ctx, "error_detail", "downstream target unavailable")
		return
	}
	// ok, we got a response, let's pass it along
	defer resp.Body.Close()
	// copy over headers
	for header, vals := range resp.Header {
		w.Header().Set(header, strings.Join(vals, ","))
	}
	// copy over status code
	w.WriteHeader(resp.StatusCode)
	// copy over body
	io.Copy(w, resp.Body)
}

func (a *app) shouldRateLimit(ctx context.Context, method, key string) error {
	ctx, span := beeline.StartSpan(ctx, "shouldRateLimit")
	defer span.Send()
	a.Lock()
	defer a.Unlock()

	var b *leakybucket.Bucket
	b, ok := a.rateLimiter[method+key]
	if !ok {
		beeline.AddField(ctx, "createBucket", method)
		if method == "GET" {
			b = &leakybucket.Bucket{
				Capacity:    getBurstLimit,
				DrainAmount: getThroughputLimit,
				DrainPeriod: 1 * time.Second,
			}
		} else {
			b = &leakybucket.Bucket{
				Capacity:    otherBurstLimit,
				DrainAmount: otherThroughputLimit,
				DrainPeriod: 1 * time.Second,
			}
		}
		a.rateLimiter[method+key] = b
	}
	return b.Add()
}
