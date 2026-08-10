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

// version ถูก stamp ตอน build ด้วย -ldflags "-X main.version=..."
// ค่าที่เห็นใน log ตอน start คือสิ่งเดียวที่บอกได้ว่า image ที่รันอยู่คือ build ไหน
var version = "dev"

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
		orphanInterval     time.Duration
		orphanGrace        time.Duration
		orphanDelete       bool
	)

	// ค่าสามตัวนี้ต่างกันทุกคลัสเตอร์ จึงรับผ่าน env ได้ด้วย (flag ชนะถ้าระบุทั้งคู่)
	// ทำแบบนี้ทำให้ manifest เป็นไฟล์คงที่ที่ apply ซ้ำได้โดยไม่ต้องแก้ —
	// การต้องแก้ไฟล์ก่อน apply ทุกครั้งคือต้นเหตุที่ค่า placeholder หลุดขึ้น production
	flag.StringVar(&cfg.ClusterID, "cluster-id", envOr("CLUSTER_ID", ""), "unique id for this cluster; scopes CLB ownership tags [env CLUSTER_ID]")
	flag.StringVar(&cfg.Region, "region", envOr("TENCENTCLOUD_REGION", ""), "Tencent Cloud region [env TENCENTCLOUD_REGION]")
	flag.StringVar(&cfg.VpcID, "vpc-id", envOr("VPC_ID", ""), "VPC that the cluster nodes live in [env VPC_ID]")
	flag.StringVar(&cfg.DefaultSubnetID, "default-subnet-id", envOr("DEFAULT_SUBNET_ID", ""), "subnet used for internal load balancers when the Service does not specify one [env DEFAULT_SUBNET_ID]")
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

	flag.DurationVar(&orphanInterval, "orphan-gc-interval", time.Hour, "how often to look for CLBs whose Service is gone; 0 disables")
	flag.DurationVar(&orphanGrace, "orphan-gc-grace-period", 30*time.Minute, "how long a CLB must look orphaned before it is eligible for deletion")
	// ค่าเริ่มต้นคือรายงานอย่างเดียว การลบ cloud resource อัตโนมัติต้องเป็นการตัดสินใจ
	// ที่มีคนกดเอง ไม่ใช่สิ่งที่ได้มาฟรีจากการอัปเกรดเวอร์ชัน
	flag.BoolVar(&orphanDelete, "orphan-gc-delete", false, "actually delete orphaned CLBs instead of only logging them")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// log เวอร์ชันก่อนทำอย่างอื่น — ถ้า startup พังทีหลัง เราจะยังรู้ว่า
	// image ไหนพัง ซึ่งเป็นข้อมูลแรกที่ต้องการเสมอเวลา debug
	setupLog.Info("k3s-tencent-clb-controller", "version", version)

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
		// อ่าน Service สดๆ ตอนถอน finalizer ไม่งั้น retry หลังชน conflict
		// จะได้ของเก่าตัวเดิมจาก cache กลับมาแล้วชนซ้ำ
		APIReader: mgr.GetAPIReader(),
	}
	if err := reconciler.SetupWithManager(mgr, maxConcurrent); err != nil {
		return fmt.Errorf("setting up service controller: %w", err)
	}

	if orphanInterval > 0 {
		// GetAPIReader ไม่ผ่าน cache — จำเป็น เพราะ cache ที่ยัง sync ไม่เสร็จ
		// จะทำให้ Service ทุกตัวดูเหมือนหายไป แล้ว GC ลบ CLB ทิ้งทั้งคลัสเตอร์
		if err := mgr.Add(&controller.OrphanCollector{
			CLB:         clbClient,
			Config:      &cfg,
			Reader:      mgr.GetAPIReader(),
			Interval:    orphanInterval,
			GracePeriod: orphanGrace,
			Delete:      orphanDelete,
		}); err != nil {
			return fmt.Errorf("setting up orphan collector: %w", err)
		}
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

// envOr อ่านค่าจาก env ใช้เป็น default ของ flag
// เว้นวรรคหัวท้ายถูกตัดทิ้ง เพราะค่าที่มาจาก ConfigMap ที่คนพิมพ์เองมักติดมาโดยไม่ตั้งใจ
// แล้วทำให้ region หรือ vpc-id ผิดแบบที่มองไม่เห็นใน log
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
