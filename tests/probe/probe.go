package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/howardjohn/pilot-load/pkg/flag"
	"github.com/howardjohn/pilot-load/pkg/kube"
	"github.com/howardjohn/pilot-load/pkg/simulation/model"
	"github.com/howardjohn/pilot-load/pkg/victoria"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/sleep"
	"istio.io/istio/pkg/test/util/tmpl"
)

type Config struct {
	Gateways     []string
	GracePeriod  time.Duration
	VictoriaLogs string
	Routes       int
	InitialRoute int
	Burst        bool
}

func main() {
	flag.RunMain(Command)
}

func Command(f *pflag.FlagSet) flag.Command {
	cfg := Config{
		Routes: 10,
	}

	flag.Register(f, &cfg.Gateways, "gateways", "list of gateways to use").Required()
	flag.Register(f, &cfg.Routes, "routes", "number of routes")
	flag.Register(f, &cfg.InitialRoute, "initial-route", "which route to start tests on")
	flag.Register(f, &cfg.VictoriaLogs, "victoria", "victoria-logs address")
	flag.Register(f, &cfg.GracePeriod, "gracePeriod", "delay between each application")
	flag.Register(f, &cfg.Burst, "burst", "apply routes back to back without waiting for traffic; probe them in order in the background")
	return flag.Command{
		Name:        "gatewayapi-probe",
		Description: "apply routes and measure time until traffic is accepted",
		Build: func(args *model.Args) (model.DebuggableSimulation, error) {
			st := map[types.NamespacedName]*Watcher{}
			for _, gw := range cfg.Gateways {
				t := parseNamespacedName(gw)
				st[t] = &Watcher{
					Name:    t,
					Client:  &http.Client{},
					Last:    0,
					Samples: nil,
				}
			}
			return &ProbeTest{Config: cfg, State: st}, nil
		},
	}
}

func parseNamespacedName(gw string) types.NamespacedName {
	ns, name, _ := strings.Cut(gw, "/")
	return types.NamespacedName{Namespace: ns, Name: name}
}

type ProbeTest struct {
	Config   Config
	State    map[types.NamespacedName]*Watcher
	reportMu sync.Mutex
}

var _ model.Simulation = &ProbeTest{}

const backendTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
spec:
  selector:
    matchLabels:
      app: backend
  template:
    metadata:
      labels:
        app: backend
    spec:
      containers:
      - name: backend
        image: howardjohn/hyper-server
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
---
apiVersion: v1
kind: Service
metadata:
  name: backend
spec:
  selector:
    app: backend
  ports:
  - name: http
    port: 80
    targetPort: 8080

`

const cfgTemplate = `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: {{.Index}}-route
  namespace: {{.Namespace}}
spec:
  hostnames:
    - {{.Index}}-route.example.com
  parentRefs:
  {{ range $gw := .Gateways }}
  {{ $spl := split "/" $gw }}
  - name: {{$spl._1}}
    namespace: {{$spl._0}}
  {{ end }}
  rules:
    - backendRefs:
        - name: backend
          port: 80
      filters:
      - type: RequestHeaderModifier
        requestHeaderModifier:
          add:
          - name: added-header
            value: added-value
      - type: ResponseHeaderModifier
        responseHeaderModifier:
          add:
          - name: added-resp-header
            value: added-resp-value
      matches:
        - path:
            type: PathPrefix
            value: /{{.Index}}
    - backendRefs:
        - name: invalid-backend
          port: 80
      filters:
      - type: RequestHeaderModifier
        requestHeaderModifier:
          add:
          - name: added-header
            value: added-value
      - type: ResponseHeaderModifier
        responseHeaderModifier:
          add:
          - name: added-resp-header
            value: added-resp-value
      matches:
        - path:
            type: PathPrefix
            value: /
`

func (a *ProbeTest) GetConfig() any {
	return a.Config
}

func (a *ProbeTest) Run(ctx model.Context) error {
	gtws := kclient.New[*gateway.Gateway](ctx.Client)
	// routes := kclient.New[*gateway.HTTPRoute](ctx.Client)
	ctx.Client.RunAndWait(ctx.Done())
	for _, gw := range a.State {
		g := gtws.Get(gw.Name.Name, gw.Name.Namespace)
		if g == nil {
			return fmt.Errorf("gateway %v not found", gw.Name)
		}
		a := g.Status.Addresses
		if len(a) == 0 {
			return fmt.Errorf("gateway %v has no address", gw.Name)
		}
		gw.Address = a[0].Value
	}

	if err := kube.ApplyTemplate(ctx.Client, "default", backendTemplate, nil); err != nil {
		return err
	}
	if a.Config.Burst {
		return a.runBurst(ctx)
	}
	for r := range a.Config.Routes {
		if err := a.applyRoute(ctx, r); err != nil {
			return err
		}
		if r == a.Config.InitialRoute-1 {
			log.Infof("applied last route, waiting...")
			time.Sleep(time.Second * 10)
		}
		if r < a.Config.InitialRoute {
			log.Infof("skipping, initial route not yet hit")
			continue
		}
		g := errgroup.Group{}
		t0 := time.Now()
		for _, gw := range a.State {
			hostname := fmt.Sprintf("%d-route.example.com", r)
			g.Go(func() error {
				if err := gw.Probe(ctx.Context, hostname, r, t0); err != nil {
					return fmt.Errorf("%v: %v", gw.Name, err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		// Periodically report
		if r%50 == 0 {
			a.Report()
		}
	}

	ctx.Cancel()
	return nil
}

func (a *ProbeTest) applyRoute(ctx model.Context, r int) error {
	type cfg struct {
		Index     int
		Namespace string
		Gateways  []string
	}
	data := cfg{
		Index:     r,
		Namespace: "default",
		Gateways:  a.Config.Gateways,
	}
	return kube.ApplyTemplate(ctx.Client, "default", cfgTemplate, data)
}

type appliedRoute struct {
	Index int
	Time  time.Time
}

// runBurst applies all routes back to back, without waiting for any to be ready. Each gateway probes
// the routes in order in the background, starting route N only once route N has been applied, and
// measures latency from when route N was applied. A gateway that keeps up finishes right after the
// last apply; a slow one builds up a backlog.
func (a *ProbeTest) runBurst(ctx model.Context) error {
	for r := range a.Config.InitialRoute {
		if err := a.applyRoute(ctx, r); err != nil {
			return err
		}
	}
	if a.Config.InitialRoute > 0 {
		log.Infof("applied initial routes, waiting...")
		time.Sleep(time.Second * 10)
	}

	// Each gateway gets its own queue of applied routes, so a slow gateway doesn't block the others
	queues := map[types.NamespacedName]chan appliedRoute{}
	for name := range a.State {
		queues[name] = make(chan appliedRoute, a.Config.Routes)
	}

	g := errgroup.Group{}
	for name, gw := range a.State {
		q := queues[name]
		g.Go(func() error {
			for ar := range q {
				hostname := fmt.Sprintf("%d-route.example.com", ar.Index)
				if err := gw.Probe(ctx.Context, hostname, ar.Index, ar.Time); err != nil {
					return fmt.Errorf("%v: %v", gw.Name, err)
				}
			}
			log.WithLabels("gateway", gw.Name).Infof("all routes ready")
			return nil
		})
	}

	done := make(chan struct{})
	go func() {
		// Periodically report while probes are running
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Second):
				a.Report()
			}
		}
	}()

	start := time.Now()
	var applyErr error
	for r := a.Config.InitialRoute; r < a.Config.Routes; r++ {
		if err := a.applyRoute(ctx, r); err != nil {
			applyErr = err
			break
		}
		applied := appliedRoute{Index: r, Time: time.Now()}
		for _, q := range queues {
			q <- applied
		}
	}
	for _, q := range queues {
		close(q)
	}
	log.Infof("applied all routes in %v", time.Since(start))

	err := g.Wait()
	close(done)
	if applyErr != nil {
		return applyErr
	}
	if err != nil {
		return err
	}
	ctx.Cancel()
	return nil
}

func (a *ProbeTest) Cleanup(ctx model.Context) error {
	a.Report()

	for r := range a.Config.Routes {
		type cfg struct {
			Index     int
			Namespace string
			Gateways  []string
		}
		spec := tmpl.MustEvaluate(cfgTemplate, cfg{
			Index:     r,
			Namespace: "default",
			Gateways:  a.Config.Gateways,
		})
		if err := kube.DeleteRaw(ctx.Client, "default", spec); err != nil {
			return nil
		}
	}
	return nil
}

func (a *ProbeTest) AllEqual(key types.NamespacedName, want int) error {
	processed := func(w *Watcher) error {
		if w.Samples == nil {
			return fmt.Errorf("%v not initialized", w.Name)
		}
		if w.Last != want {
			return fmt.Errorf("want %d, got %d for %v", want, w.Last, w.Name)
		}
		return nil
	}
	// Check the one we are currently processing to avoid confusing logs
	if err := processed(a.State[key]); err != nil {
		return err
	}
	for _, w := range a.State {
		if err := processed(w); err != nil {
			return err
		}
	}
	return nil
}

func (a *ProbeTest) Report() {
	// Burst mode reports from a background goroutine; don't let two reports send the same samples
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	for _, gw := range a.State {
		totalErrors := 0
		totalLatency := time.Duration(0)
		maxLatency := time.Duration(0)
		gw.mu.Lock()
		for _, sample := range gw.Samples {
			totalErrors += sample.Errors
			totalLatency += sample.Latency
			maxLatency = max(maxLatency, sample.Latency)
		}
		gw.mu.Unlock()
		// TODO: average latency, total latency, max
		log.WithLabels("gateway", gw.Name, "errors", totalErrors, "runtime", totalLatency, "max", maxLatency).Info("test complete")
	}

	if a.Config.VictoriaLogs != "" {
		test := "probe"
		if a.Config.Burst {
			test = "probe-burst"
		}
		var entries []VicLogEntry
		reported := map[*Watcher]int{}
		for name, w := range a.State {
			w.mu.Lock()
			samples := w.Samples[w.Reported:]
			reported[w] = len(w.Samples)
			w.mu.Unlock()
			// Only send samples not already reported; Report is called periodically
			for _, sample := range samples {
				t := sample.Time
				if a.Config.Burst {
					// Burst gateways can complete many routes at the same instant, which would stack them on
					// one point; plot by when the route was applied instead.
					t = sample.Start
				}
				entries = append(entries, VicLogEntry{
					Message: "event",
					Test:    test,
					Gateway: name.String(),
					Time:    t.UnixNano(),
					Value:   sample.Latency.Microseconds(),
					Iter:    sample.Iter,
				})
			}
		}
		if err := victoria.Report(a.Config.VictoriaLogs, []string{"gateway", "test"}, entries); err != nil {
			log.Errorf("failed to report victoria logs: %v", err)
		} else {
			for w, n := range reported {
				w.mu.Lock()
				w.Reported = n
				w.mu.Unlock()
			}
			log.Infof("reported victoria logs")
		}
	}
}

type VicLogEntry struct {
	Message string `json:"_msg"`
	Gateway string `json:"gateway"`
	Test    string `json:"test"`
	Time    int64  `json:"_time"`
	Value   int64  `json:"value"` // Microseconds
	Iter    int    `json:"iter"`
}

type Watcher struct {
	Name    types.NamespacedName
	Client  *http.Client
	Address string
	Last    int
	Samples []Sample
	// Reported is the number of Samples already sent to VictoriaLogs
	Reported int
	// mu guards Samples and Reported, which are reported while burst probes are running
	mu sync.Mutex
}

// Probe waits until hostname returns 200, recording the latency since t0
func (w *Watcher) Probe(ctx context.Context, hostname string, r int, t0 time.Time) error {
	log := log.WithLabels("gateway", w.Name.String(), "iter", r)
	delay := time.Millisecond * 5
	errors := 0
	for range 1000000 {
		url := fmt.Sprintf("http://%s/%d", w.Address, r)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}
		req.Host = hostname
		resp, err := w.Client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		c := resp.StatusCode
		log.Debugf("probe result: %v", c)
		switch c {
		case 404:
			sleep.UntilContext(ctx, delay)
			continue
		case 200:
			tn := time.Now()
			w.mu.Lock()
			w.Samples = append(w.Samples, Sample{
				Start:   t0,
				Time:    time.Now(),
				Latency: tn.Sub(t0),
				Iter:    r,
				Errors:  errors,
			})
			w.mu.Unlock()
			// success!
			// Todo continue a few to ensure we get 200 consistently
			log.WithLabels("latency", tn.Sub(t0)).Infof("probe completed: %v", c)
			return nil
		default:
			log.Errorf("unexpected status code: %v", c)
			sleep.UntilContext(ctx, delay)
			errors++
			continue
		}
	}
	return fmt.Errorf("timed out waiting for %v", w)
}

type Sample struct {
	Iter int
	// Start is when probing began (burst mode: when the route was applied)
	Start   time.Time
	Time    time.Time
	Latency time.Duration
	Errors  int
}
