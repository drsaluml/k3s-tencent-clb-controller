package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/config"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/controller"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/node"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfg                config.Config
		metricsAddr        string
		probeAddr          string
		leaderElection     bool
		leaderElectionNS   string
		maxConcurrent      int
		excludedNodeLabels string
		apiQPS             float64
		nodeCacheTTL       time.Duration
	)

	flag.StringVar(&cfg.ClusterID, "cluster-id", "", "unique id for this cluster; scopes CLB ownership tags")
	flag.StringVar(&cfg.Region, "region", os.Getenv("TENCENTCLOUD_REGION"), "Tencent Cloud region")
	flag.StringVar(&cfg.VpcID, "vpc-id", "", "VPC that the cluster nodes live in")
	flag.StringVar(&cfg.DefaultSubnetID, "default-subnet-id", "", "subnet used for internal load balancers when the Service does not specify one")
	flag.StringVar(&cfg.ClassExternal, "loadbalancer-class-external", config.DefaultClassExternal, "spec.loadBalancerClass handled as a public CLB")
	flag.StringVar(&cfg.ClassInternal, "loadbalancer-class-internal", config.DefaultClassInternal, "spec.loadBalancerClass handled as an internal CLB")
	flag.DurationVar(&cfg.ResyncPeriod, "resync-period", 10*time.Minute, "how often to re-check every managed CLB for drift")
	flag.StringVar(&excludedNodeLabels, "excluded-node-labels", "", "comma-separated key=value labels; matching nodes are never registered as backends")

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "")
	flag.BoolVar(&leaderElection, "leader-elect", true, "")
	flag.StringVar(&leaderElectionNS, "leader-election-namespace", "kube-system", "")
	flag.IntVar(&maxConcurrent, "concurrent-service-syncs", 2, "")
	flag.Float64Var(&apiQPS, "cloud-api-qps", 10, "client-side rate limit for Tencent Cloud API calls")
	flag.DurationVar(&nodeCacheTTL, "node-cache-ttl", time.Hour, "how long a node→CVM instance mapping stays cached")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg.Defaults()
	cfg.ExcludedNodeLabels = parseLabels(excludedNodeLabels)
	if err := cfg.Validate(); err != nil {
		return err
	}

	secretID := os.Getenv("TENCENTCLOUD_SECRET_ID")
	secretKey := os.Getenv("TENCENTCLOUD_SECRET_KEY")
	if secretID == "" || secretKey == "" {
		return fmt.Errorf("TENCENTCLOUD_SECRET_ID and TENCENTCLOUD_SECRET_KEY must be set")
	}

	clbClient, err := clb.New(clb.Options{
		SecretID:  secretID,
		SecretKey: secretKey,
		Region:    cfg.Region,
		QPS:       apiQPS,
	})
	if err != nil {
		return err
	}

	nodeResolver, err := node.NewResolver(node.Options{
		SecretID:  secretID,
		SecretKey: secretKey,
		Region:    cfg.Region,
		TTL:       nodeCacheTTL,
	})
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElection,
		LeaderElectionID:        "clb-controller.tencentcloud.com",
		LeaderElectionNamespace: leaderElectionNS,
		// จงใจไม่ใส่ field selector "spec.type=LoadBalancer" ลงใน cache
		// field selector ตัวนั้นไม่ได้รองรับทุกเวอร์ชันของ apiserver และถ้าไม่รองรับ
		// informer จะ fail ตอน start ทั้งตัว — แลกกับ memory ที่ประหยัดได้ไม่คุ้ม
		// การกรองจริงเกิดที่ predicate ซึ่งกันงานไม่จำเป็นออกจาก workqueue อยู่แล้ว
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	reconciler := &controller.ServiceReconciler{
		Client:   mgr.GetClient(),
		CLB:      clbClient,
		Nodes:    nodeResolver,
		Recorder: mgr.GetEventRecorderFor("clb-controller"),
		Config:   &cfg,
	}
	if err := reconciler.SetupWithManager(mgr, maxConcurrent); err != nil {
		return fmt.Errorf("setting up service controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	setupLog.Info("starting clb controller",
		"cluster", cfg.ClusterID, "region", cfg.Region, "vpc", cfg.VpcID)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found {
			continue
		}
		out[k] = v
	}
	return out
}
