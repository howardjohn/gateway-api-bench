package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/howardjohn/pilot-load/pkg/flag"
	"github.com/howardjohn/pilot-load/pkg/kube"
	"github.com/howardjohn/pilot-load/pkg/simulation/model"
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
	Gateways    []string
	GracePeriod time.Duration
	Iterations  int
}

func main() {
	flag.RunMain(Command)
}

func Command(f *pflag.FlagSet) flag.Command {
	cfg := Config{
		Iterations:  10,
		GracePeriod: time.Millisecond * 200,
	}

	flag.Register(f, &cfg.Gateways, "gateways", "list of gateways to use").Required()
	flag.Register(f, &cfg.Iterations, "iterations", "number of changes to make")
	flag.Register(f, &cfg.GracePeriod, "gracePeriod", "delay between each change")
	return flag.Command{
		Name:        "gatewayapi-routechange",
		Description: "change routes and ensure traffic is continually successful",
		Build: func(args *model.Args) (model.DebuggableSimulation, error) {
			st := map[types.NamespacedName]*Watcher{}
			for _, gw := range cfg.Gateways {
				t := parseNamespacedName(gw)
				st[t] = &Watcher{
					Name:   t,
					Client: &http.Client{},
				}
			}
			return &ChangeTest{Config: cfg, State: st}, nil
		},
	}
}

func parseNamespacedName(gw string) types.NamespacedName {
	ns, name, _ := strings.Cut(gw, "/")
	return types.NamespacedName{Namespace: ns, Name: name}
}

type ChangeTest struct {
	Config Config
	State  map[types.NamespacedName]*Watcher
}

var _ model.Simulation = &ChangeTest{}

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
        env:
        - name: PORT
          value: 8080,8081
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
  - name: http-alt
    port: 8080
    targetPort: 8081
`

const backendChangeTemplate = `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route
  namespace: {{.Namespace}}
spec:
  hostnames:
    - route.example.com
  parentRefs:
  {{ range $gw := .Gateways }}
  {{ $spl := split "/" $gw }}
  - name: {{$spl._1}}
    namespace: {{$spl._0}}
  {{ end }}
  rules:
    - backendRefs:
        - name: backend
          port: {{ if .Rand }}80{{ else }}8080{{end}}
{{ if .Rand }}
          filters:
          - type: ResponseHeaderModifier
            responseHeaderModifier:
              add:
              - name: my-added-header
                value: added-value
{{end}}
`

func (a *ChangeTest) GetConfig() any {
	return a.Config
}

type cfg struct {
	Rand      bool
	Namespace string
	Gateways  []string
}

func (a *ChangeTest) Run(ctx model.Context) error {
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

	if err := kube.ApplyTemplate(ctx.Client, "default", backendTemplate, nil); err != nil {
		return err
	}

	data := cfg{
		Namespace: "default",
		Gateways:  a.Config.Gateways,
	}
	log.Infof("applying initial route...")
	if err := kube.ApplyTemplate(ctx.Client, "default", backendChangeTemplate, data); err != nil {
		return err
	}

	g := errgroup.Group{}
	done := make(chan error, len(a.State))
	for _, gw := range a.State {
		// Wait for propagation...
		if err := gw.AwaitReady(ctx, "route.example.com"); err != nil {
			return err
		}
		g.Go(func() error {
			err := gw.Probe(ctx, "route.example.com")
			if err != nil {
				done <- err
			}
			return err
		})
	}
	for r := range a.Config.Iterations {
		data := cfg{
			Rand:      r%2 == 0,
			Namespace: "default",
			Gateways:  a.Config.Gateways,
		}
		log.Infof("changing route %d...", r)
		if err := kube.ApplyTemplate(ctx.Client, "default", backendChangeTemplate, data); err != nil {
			return err
		}
		exit := false
		select {
		case <-done:
			exit = true
		default:
		}
		if exit {
			time.Sleep(time.Second)
			break
		}
		if !sleep.UntilContext(ctx.Context, a.Config.GracePeriod) {
			return fmt.Errorf("context cancelled")
		}
	}
	ctx.Cancel()
	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func (a *ChangeTest) Cleanup(ctx model.Context) error {
	a.Report()
	spec := tmpl.MustEvaluate(backendChangeTemplate, cfg{
		Namespace: "default",
		Gateways:  a.Config.Gateways,
	})
	if err := kube.DeleteRaw(ctx.Client, "default", spec); err != nil {
		return nil
	}

	return nil
}

func (a *ChangeTest) Report() {
	for _, gw := range a.State {
		// TODO: average latency, total latency, max
		log.WithLabels("gateway", gw.Name, "requests", gw.Iters).Info("test complete")
	}
}

type Watcher struct {
	Name    types.NamespacedName
	Client  *http.Client
	Address string
	Iters   int
}

func (w *Watcher) AwaitReady(ctx context.Context, hostname string) error {
	delay := time.Millisecond * 25
	for {
		w.Iters++
		url := fmt.Sprintf("http://%s/%d", w.Address, w.Iters)
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
		if resp.StatusCode == 200 {
			return nil
		}
		if !sleep.UntilContext(ctx, delay) {
			return fmt.Errorf("context cancelled")
		}
	}
	return fmt.Errorf("route never became ready")
}

func (w *Watcher) Probe(ctx context.Context, hostname string) error {
	log := log.WithLabels("gateway", w.Name.String())
	delay := time.Microsecond
	for {
		t0 := time.Now()
		w.Iters++
		url := fmt.Sprintf("http://%s/%d", w.Address, w.Iters)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}
		req.Host = hostname
		resp, err := w.Client.Do(req)
		if err != nil {
			return err
		}
		c := resp.StatusCode
		if c != 200 {
			res, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("unexpected status code on iteration %d for gateway %v: %d. Body:\n%v", w.Iters, w.Name, c, string(res))
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.WithLabels("latency", time.Since(t0)).Debugf("probe completed: %v", c)
		if !sleep.UntilContext(ctx, delay) {
			return nil
		}
	}
}
