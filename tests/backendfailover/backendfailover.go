package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
	Iterations   int
	VictoriaLogs string
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
	flag.Register(f, &cfg.VictoriaLogs, "victoria", "victoria-logs address")
	flag.Register(f, &cfg.GracePeriod, "gracePeriod", "delay between each change")
	return flag.Command{
		Name:        "gatewayapi-backendfailover",
		Description: "Make a backend fail and see what happens",
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
apiVersion: v1
kind: Pod
metadata:
  name: backend-unhealthy
  namespace: default
  labels:
    app: backend
    mode: unhealthy
  annotations:
    prometheus.io/port: "9999"
    prometheus.io/scrape: "true"
spec:
  containers:
  - name: backend
    image: gcr.io/istio-testing/app
    args: [--metrics=9999]
    securityContext:
      runAsUser: 0
      capabilities:
        add:
        - NET_ADMIN
        - NET_RAW
    resources:
      requests:
        memory: "64Mi"
        cpu: "100m"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend-healthy
  namespace: default
spec:
  selector:
    matchLabels:
      app: backend
      mode: healthy
  replicas: 3
  template:
    metadata:
      labels:
        app: backend
        mode: healthy
      annotations:
        prometheus.io/port: "9999"
        prometheus.io/scrape: "true"
    spec:
      containers:
      - name: backend
        image: gcr.io/istio-testing/app
        args: [--metrics=9999]
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
---
apiVersion: v1
kind: Service
metadata:
  name: backend
  namespace: default
spec:
  selector:
    app: backend
  ports:
  - name: http
    port: 80
    targetPort: 8080
---
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: outlier
  namespace: default
spec:
  host: backend
  trafficPolicy:
    outlierDetection:
      baseEjectionTime: 10s
      consecutive5xxErrors: 5
      consecutiveGatewayErrors: 5
      consecutiveLocalOriginFailures: 5
      maxEjectionPercent: 100
      splitExternalLocalOriginErrors: true
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: outlier
  namespace: envoy
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: envoy-gateway
  healthCheck:
    passive:
      baseEjectionTime: 10s
      consecutive5XxErrors: 5
      consecutiveGatewayErrors: 5
      consecutiveLocalOriginFailures: 5
      maxEjectionPercent: 100
      splitExternalLocalOriginErrors: true
    panicThreshold: 0
    active:
      http:
        path: /healthz
        expectedStatuses: [200]
      healthyThreshold: 1
      unhealthyThreshold: 1
      interval: 1s
      timeout: 1s
      type: HTTP
`

const routeTemplate = `
{{ range $rr := until 16 }}
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route-{{$rr}}
  namespace: {{$.Namespace}}
spec:
  hostnames:
    - route.example.com
  parentRefs:
  {{ range $gw := $.Gateways }}
  {{ $spl := split "/" $gw }}
  - name: {{$spl._1}}
    namespace: {{$spl._0}}
  {{ end }}
  rules:
{{ range $r := until 16 }}
    - backendRefs:
        - name: backend
          port: 80      
      matches:
      - path:
          type: PathPrefix
          value: /{{add (mul 16 $rr) $r}}
      retry:
        attempts: 0
{{ end }}
---
{{ end }}
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

	if err := kube.ApplyTemplate(ctx.Client, "", backendTemplate, nil); err != nil {
		return err
	}
	_ = exec.Command("kubectl", "exec", "--namespace=default", "backend-unhealthy", "--", "iptables", "-D", "INPUT", "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset").Run()

	data := cfg{
		Namespace: "default",
		Gateways:  a.Config.Gateways,
	}
	log.Infof("applying initial route...")
	if err := kube.ApplyTemplate(ctx.Client, "default", routeTemplate, data); err != nil {
		return err
	}

	reporter := victoria.NewBatchReporter[VicLogEntry](a.Config.VictoriaLogs)
	defer reporter.Close()

	g := errgroup.Group{}
	done := make(chan error, len(a.State))
	for _, gw := range a.State {
		// Wait for propagation...
		if err := gw.AwaitReady(ctx, "route.example.com"); err != nil {
			return err
		}
	}
	log.Infof("all gateways are ready... probing")
	for _, gw := range a.State {
		g.Go(func() error {
			err := gw.Probe(ctx, "route.example.com", reporter)
			if err != nil {
				done <- err
			}
			return err
		})
	}
	for iter := range 5 {
		log := log.WithLabels("iter", iter)
		sleep.UntilContext(ctx, time.Second*22)
		c := exec.Command("kubectl", "exec", "--namespace=default", "backend-unhealthy", "--", "iptables", "-A", "INPUT", "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset")
		c.Stderr = os.Stderr
		c.Stdout = os.Stdout

		if err := c.Run(); err != nil {
			return err
		}
		log.Infof("pod marked unhealthy")

		sleep.UntilContext(ctx, time.Second*22)
		c = exec.Command("kubectl", "exec", "--namespace=default", "backend-unhealthy", "--", "iptables", "-D", "INPUT", "-p", "tcp", "-j", "REJECT", "--reject-with", "tcp-reset")
		c.Stderr = os.Stderr
		c.Stdout = os.Stdout

		if err := c.Run(); err != nil {
			return err
		}
		log.Infof("pod marked healthy")

	}
	log.Infof("done!")
	ctx.Cancel()
	return nil
}

func (a *ChangeTest) Cleanup(ctx model.Context) error {
	spec := tmpl.MustEvaluate(routeTemplate, cfg{
		Namespace: "default",
		Gateways:  a.Config.Gateways,
	})
	if err := kube.DeleteRaw(ctx.Client, "default", spec); err != nil {
		return nil
	}

	return nil
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
		log.Infof("probing %v: %v", hostname, resp.StatusCode)
		if resp.StatusCode == 200 {
			return nil
		}
		if !sleep.UntilContext(ctx, delay) {
			return fmt.Errorf("context cancelled")
		}
	}
	return fmt.Errorf("route never became ready")
}

var HostnameRegex = regexp.MustCompile("Hostname=(.*)")

func (w *Watcher) Probe(ctx context.Context, hostname string, reporter *victoria.BatchReporter[VicLogEntry]) error {
	log := log.WithLabels("gateway", w.Name.String())
	t := time.NewTicker(time.Millisecond * 5)
	for {
		t0 := time.Now()
		w.Iters++
		//url := fmt.Sprintf("http://%s/%d", w.Address, 1)
		url := fmt.Sprintf("http://%s/%d", w.Address, w.Iters%256)
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
		lg := VicLogEntry{
			Message: "request",
			Test:    "backendfailover",
			Gateway: w.Name.String(),
			Success: true,
			Code:    0,
			Time:    time.Now().UnixNano(),
		}
		if c != 200 {
			lg.Code = 1
			lg.Success = false
		}
		b, err := io.ReadAll(resp.Body)
		if err == nil {
			m := HostnameRegex.FindSubmatch(b)
			if m != nil {
				lg.Backend = string(m[1])
			}
		}
		reporter.Report(lg)
		resp.Body.Close()
		log.WithLabels("latency", time.Since(t0)).Debugf("probe completed: %v", c)
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil
		}
	}
}

type VicLogEntry struct {
	Message string `json:"_msg"`
	Gateway string `json:"gateway"`
	Test    string `json:"test"`
	Success bool   `json:"success"`
	Backend string `json:"backend"`
	Code    int    `json:"value"`
	Time    int64  `json:"_time"`
}
