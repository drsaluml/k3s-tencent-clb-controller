package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
)

// AnnoInstanceID เป็นทางลัด: ถ้า node มี annotation นี้ เราเชื่อเลยไม่ต้องถาม CVM API
// ใช้กับ node ที่ lookup ด้วย private IP ไม่ได้ (hybrid, node นอก VPC)
const AnnoInstanceID = "clb.tencentcloud.com/instance-id"

// ErrNotFound แปลว่าหา CVM ที่ตรงกับ node นี้ไม่เจอ
// caller ควรข้าม node นั้นแล้วไปต่อ ไม่ใช่ล้มทั้ง reconcile —
// node หนึ่งตัวที่ resolve ไม่ได้ไม่ควรทำให้ LB ทั้งตัวค้าง
var ErrNotFound = fmt.Errorf("no CVM instance found for node")

// Resolver แปลง k8s Node → CVM InstanceId
//
// ทำไมต้องมีชั้นนี้: CLB RegisterTargets รับ InstanceId (ins-xxxx) ไม่ใช่ IP
// แต่ K3s ตั้ง node.spec.providerID เป็น "k3s://<node-name>" จาก embedded CCM ของมันเอง
// ซึ่งใช้หา CVM ไม่ได้ ต่างจากคลัสเตอร์ที่รัน Tencent CCM เต็มตัว
type Resolver interface {
	InstanceID(ctx context.Context, n *corev1.Node) (string, error)
	Forget(nodeName string)
}

type cacheEntry struct {
	instanceID string
	internalIP string // เก็บไว้เช็คว่า node เปลี่ยน IP หรือยัง
	expiresAt  time.Time
}

type resolver struct {
	cvm     *cvm.Client
	limiter *rate.Limiter
	ttl     time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry

	now func() time.Time
}

type Options struct {
	SecretID  string
	SecretKey string
	Region    string
	// TTL ของ cache — CVM API มี rate limit ต่ำกว่า CLB
	// การถามทุก reconcile จะชน limit เร็วมากในคลัสเตอร์ที่ node เยอะ
	TTL   time.Duration
	QPS   float64
	Burst int
}

func NewResolver(opts Options) (Resolver, error) {
	if opts.TTL == 0 {
		opts.TTL = time.Hour
	}
	if opts.QPS == 0 {
		opts.QPS = 5
	}
	if opts.Burst == 0 {
		opts.Burst = 10
	}

	cred := common.NewCredential(opts.SecretID, opts.SecretKey)
	cpf := profile.NewClientProfile()
	cpf.HttpProfile.ReqTimeout = 30
	cpf.Language = "en-US"

	c, err := cvm.NewClient(cred, opts.Region, cpf)
	if err != nil {
		return nil, fmt.Errorf("creating cvm client: %w", err)
	}
	return &resolver{
		cvm:     c,
		limiter: rate.NewLimiter(rate.Limit(opts.QPS), opts.Burst),
		ttl:     opts.TTL,
		cache:   map[string]cacheEntry{},
		now:     time.Now,
	}, nil
}

func (r *resolver) InstanceID(ctx context.Context, n *corev1.Node) (string, error) {
	// 1. annotation ชนะทุกอย่าง — ทางหนีไฟที่ user ตั้งเองได้
	if id := n.Annotations[AnnoInstanceID]; id != "" {
		return id, nil
	}

	ip := internalIP(n)
	if ip == "" {
		return "", fmt.Errorf("node %s has no InternalIP: %w", n.Name, ErrNotFound)
	}

	// 2. cache — invalidate เมื่อ node เปลี่ยน IP
	r.mu.RLock()
	e, ok := r.cache[n.Name]
	r.mu.RUnlock()
	if ok && e.internalIP == ip && r.now().Before(e.expiresAt) {
		return e.instanceID, nil
	}

	// 3. ถาม CVM ด้วย private IP
	id, err := r.lookupByPrivateIP(ctx, ip)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	r.cache[n.Name] = cacheEntry{instanceID: id, internalIP: ip, expiresAt: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return id, nil
}

func (r *resolver) Forget(nodeName string) {
	r.mu.Lock()
	delete(r.cache, nodeName)
	r.mu.Unlock()
}

func (r *resolver) lookupByPrivateIP(ctx context.Context, ip string) (string, error) {
	req := cvm.NewDescribeInstancesRequest()
	req.Filters = []*cvm.Filter{{
		Name:   common.StringPtr("private-ip-address"),
		Values: common.StringPtrs([]string{ip}),
	}}
	req.Limit = common.Int64Ptr(10)

	if err := r.limiter.Wait(ctx); err != nil {
		return "", err
	}
	resp, err := r.cvm.DescribeInstancesWithContext(ctx, req)
	if err != nil {
		return "", fmt.Errorf("describing CVM by private ip %s: %w", ip, err)
	}
	if resp.Response == nil || len(resp.Response.InstanceSet) == 0 {
		return "", fmt.Errorf("private ip %s: %w", ip, ErrNotFound)
	}

	// filter private-ip-address เป็น fuzzy match ฝั่ง Tencent
	// ต้องยืนยันว่า IP ตรงเป๊ะจริง ไม่งั้นอาจได้ instance ผิดตัว
	for _, inst := range resp.Response.InstanceSet {
		for _, p := range inst.PrivateIpAddresses {
			if p != nil && *p == ip {
				return *inst.InstanceId, nil
			}
		}
	}
	return "", fmt.Errorf("private ip %s: %w", ip, ErrNotFound)
}

func internalIP(n *corev1.Node) string {
	for _, addr := range n.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}
