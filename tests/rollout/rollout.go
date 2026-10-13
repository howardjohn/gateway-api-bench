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
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gateway "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/sleep"
	"istio.io/istio/pkg/test/util/tmpl"
)

type Config struct {
	Gateways   []string
	Target     string
	Iterations int
	Settle     time.Duration
	Drain      time.Duration
}

func main() {
	flag.RunMain(Command)
}

func Command(f *pflag.FlagSet) flag.Command {
	cfg := Config{
		Target:     "backend",
		Iterations: 3,
		Settle:     1 * time.Second,
		Drain:      1 * time.Second,
	}

	flag.Register(f, &cfg.Gateways, "gateways", "list of gateways to use").Required()
	flag.Register(f, &cfg.Target, "target", "what to rolling restart: 'backend' or 'gateway'")
	flag.Register(f, &cfg.Iterations, "iterations", "number of rolling restarts")
	flag.Register(f, &cfg.Settle, "settle", "how long to keep sending traffic after each rollout completes")
	flag.Register(f, &cfg.Drain, "drain", "how long terminating backend pods keep serving, i.e. how long gateways have to stop using them")
	return flag.Command{
		Name:        "gatewayapi-rollout",
		Description: "rolling restart the backend or gateway and ensure traffic is continually successful",
		Build: func(args *model.Args) (model.DebuggableSimulation, error) {
			if cfg.Target != "backend" && cfg.Target != "gateway" {
				return nil, fmt.Errorf("--target must be 'backend' or 'gateway', got %q", cfg.Target)
			}
			st := map[types.NamespacedName]*Watcher{}
			for _, gw := range cfg.Gateways {
				t := parseNamespacedName(gw)
				st[t] = &Watcher{
					Name: t,
					// Reuse connections like a real client would; a timeout catches requests hung on a dying pod
					Client: &http.Client{Timeout: 2 * time.Second},
				}
			}
			return &RolloutTest{Config: cfg, State: st}, nil
		},
	}
}

func parseNamespacedName(gw string) types.NamespacedName {
	ns, name, _ := strings.Cut(gw, "/")
	return types.NamespacedName{Namespace: ns, Name: name}
}

type RolloutTest struct {
	Config Config
	State  map[types.NamespacedName]*Watcher
}

var _ model.Simulation = &RolloutTest{}

// The backend is set up to roll gracefully: multiple replicas, never below the desired count, a readiness
// probe, and a preStop sleep (--drain) so the old pod keeps serving while gateways remove it from their
// endpoints. Any errors during a backend rollout are then down to how quickly the gateway reacts to endpoint
// changes.
const backendTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rollout-backend
spec:
  replicas: 3
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      app: rollout-backend
  template:
    metadata:
      labels:
        app: rollout-backend
    spec:
      # The grace period includes the preStop sleep. hyper-server ignores SIGTERM, so it is killed when this
      # expires; keep it just past the preStop so pods don't hang around for the default 30s.
      terminationGracePeriodSeconds: {{ add .DrainSeconds 1 }}
      containers:
      - name: backend
        image: howardjohn/hyper-server
        readinessProbe:
          tcpSocket:
            port: 8080
          periodSeconds: 1
        lifecycle:
          preStop:
            sleep:
              seconds: {{ .DrainSeconds }}
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
---
apiVersion: v1
kind: Service
metadata:
  name: rollout-backend
spec:
  selector:
    app: rollout-backend
  ports:
  - name: http
    port: 80
    targetPort: 8080
`

const routeTemplate = `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: rollout
  namespace: {{.Namespace}}
spec:
  hostnames:
    - rollout.example.com
  parentRefs:
  {{ range $gw := .Gateways }}
  {{ $spl := split "/" $gw }}
  - name: {{$spl._1}}
    namespace: {{$spl._0}}
  {{ end }}
  rules:
    - backendRefs:
        - name: rollout-backend
          port: 80
`

type cfg struct {
	Namespace string
	Gateways  []string
}

type backendCfg struct {
	DrainSeconds int
}

func (a *RolloutTest) backendData() backendCfg {
	return backendCfg{DrainSeconds: int(a.Config.Drain.Seconds())}
}

func (a *RolloutTest) GetConfig() any {
	return a.Config
}

func (a *RolloutTest) Run(ctx model.Context) error {
	gtws := kclient.New[*gateway.Gateway](ctx.Client)
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

	// Work out what we are restarting before touching anything
	var targets []types.NamespacedName
	if a.Config.Target == "backend" {
		targets = []types.NamespacedName{{Namespace: "default", Name: "rollout-backend"}}
	} else {
		for _, gw := range a.State {
			d, err := gatewayDeployment(ctx, gw.Name)
			if err != nil {
				return err
			}
			targets = append(targets, d)
		}
	}

	if err := kube.ApplyTemplate(ctx.Client, "default", backendTemplate, a.backendData()); err != nil {
		return err
	}
	if err := waitForRollout(ctx, types.NamespacedName{Namespace: "default", Name: "rollout-backend"}); err != nil {
		return err
	}
	data := cfg{Namespace: "default", Gateways: a.Config.Gateways}
	if err := kube.ApplyTemplate(ctx.Client, "default", routeTemplate, data); err != nil {
		return err
	}

	for _, gw := range a.State {
		if err := gw.AwaitReady(ctx, "rollout.example.com"); err != nil {
			return err
		}
	}

	// Traffic runs for the whole test and never stops on errors; we want to measure total downtime
	probeCtx, cancelProbes := context.WithCancel(ctx.Context)
	defer cancelProbes()
	g := errgroup.Group{}
	for _, gw := range a.State {
		g.Go(func() error {
			gw.Probe(probeCtx, "rollout.example.com")
			return nil
		})
	}

	for i := range a.Config.Iterations {
		for _, t := range targets {
			log.Infof("rollout %d: restarting %v...", i, t)
			start := time.Now()
			if err := restart(ctx, t); err != nil {
				return err
			}
			if err := waitForRollout(ctx, t); err != nil {
				return err
			}
			log.Infof("rollout %d: %v complete in %v", i, t, time.Since(start))
		}
		// Old pods may still be terminating and draining; keep sending traffic until they are gone
		if !sleep.UntilContext(ctx.Context, a.Config.Settle) {
			return fmt.Errorf("context cancelled")
		}
	}

	cancelProbes()
	_ = g.Wait()
	ctx.Cancel()
	return nil
}

// gatewayDeployment finds the data plane Deployment for a Gateway.
func gatewayDeployment(ctx model.Context, gw types.NamespacedName) (types.NamespacedName, error) {
	// Traefik is a single shared deployment, not one per Gateway
	if gw.Name == "traefik" {
		return types.NamespacedName{Namespace: "traefik-system", Name: "traefik"}, nil
	}
	// All the other implementations label their deployments with the standard label
	ds, err := ctx.Client.Kube().AppsV1().Deployments(gw.Namespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: "gateway.networking.k8s.io/gateway-name=" + gw.Name,
	})
	if err != nil {
		return types.NamespacedName{}, err
	}
	if len(ds.Items) != 1 {
		return types.NamespacedName{}, fmt.Errorf("expected 1 deployment for gateway %v, found %d", gw, len(ds.Items))
	}
	return types.NamespacedName{Namespace: gw.Namespace, Name: ds.Items[0].Name}, nil
}

// restart does the same as `kubectl rollout restart`.
func restart(ctx model.Context, d types.NamespacedName) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339Nano))
	_, err := ctx.Client.Kube().AppsV1().Deployments(d.Namespace).
		Patch(ctx.Context, d.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// waitForRollout waits until every replica is on the latest template and ready, like `kubectl rollout status`.
func waitForRollout(ctx model.Context, d types.NamespacedName) error {
	for {
		dep, err := ctx.Client.Kube().AppsV1().Deployments(d.Namespace).Get(ctx.Context, d.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if rolledOut(dep) {
			return nil
		}
		if !sleep.UntilContext(ctx.Context, 500*time.Millisecond) {
			return fmt.Errorf("context cancelled waiting for %v", d)
		}
	}
}

func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation &&
		s.UpdatedReplicas == want &&
		s.Replicas == want &&
		s.AvailableReplicas == want
}

func (a *RolloutTest) Cleanup(ctx model.Context) error {
	a.Report()
	spec := tmpl.MustEvaluate(routeTemplate, cfg{Namespace: "default", Gateways: a.Config.Gateways})
	if err := kube.DeleteRaw(ctx.Client, "default", spec); err != nil {
		return nil
	}
	if err := kube.DeleteRaw(ctx.Client, "default", tmpl.MustEvaluate(backendTemplate, a.backendData())); err != nil {
		return nil
	}
	return nil
}

func (a *RolloutTest) Report() {
	for _, gw := range a.State {
		gw.mu.Lock()
		longest := time.Duration(0)
		total := time.Duration(0)
		for _, o := range gw.Outages {
			longest = max(longest, o.End.Sub(o.Start))
			total += o.End.Sub(o.Start)
		}
		log.WithLabels(
			"gateway", gw.Name,
			"target", a.Config.Target,
			"requests", gw.Requests,
			"errors", gw.Errors,
			"outages", len(gw.Outages),
			"downtime", total,
			"longest", longest,
		).Info("test complete")
		gw.mu.Unlock()
	}
}

type Outage struct {
	Start time.Time
	End   time.Time
	// First error seen in this outage, to tell connection resets from 5xx etc
	Reason string
}

type Watcher struct {
	Name    types.NamespacedName
	Client  *http.Client
	Address string

	mu       sync.Mutex
	Requests int
	Errors   int
	Outages  []Outage
}

func (w *Watcher) AwaitReady(ctx model.Context, hostname string) error {
	for {
		if err := w.request(hostname); err == nil {
			return nil
		}
		if !sleep.UntilContext(ctx.Context, 25*time.Millisecond) {
			return fmt.Errorf("context cancelled")
		}
	}
}

func (w *Watcher) request(hostname string) error {
	req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", w.Address), nil)
	if err != nil {
		return err
	}
	req.Host = hostname
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Probe sends requests back to back until ctx is cancelled. Unlike routechange it does not stop on the
// first error: consecutive failures are grouped into an outage window, so we can report total downtime.
func (w *Watcher) Probe(ctx context.Context, hostname string) {
	log := log.WithLabels("gateway", w.Name.String())
	var current *Outage
	for ctx.Err() == nil {
		err := w.request(hostname)
		now := time.Now()
		w.mu.Lock()
		w.Requests++
		if err != nil {
			w.Errors++
			if current == nil {
				current = &Outage{Start: now, Reason: err.Error()}
				log.Warnf("outage started: %v", err)
			}
		} else if current != nil {
			current.End = now
			w.Outages = append(w.Outages, *current)
			log.Warnf("outage ended after %v", current.End.Sub(current.Start))
			current = nil
		}
		w.mu.Unlock()
		if err != nil {
			// Don't spin on fast failures (e.g. connection refused)
			sleep.UntilContext(ctx, time.Millisecond)
		}
	}
	if current != nil {
		w.mu.Lock()
		current.End = time.Now()
		w.Outages = append(w.Outages, *current)
		w.mu.Unlock()
	}
}
