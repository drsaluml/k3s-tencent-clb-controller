package clb

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/clb/v20180317"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	"golang.org/x/time/rate"
)

// Options สำหรับสร้าง client จริง
type Options struct {
	SecretID  string
	SecretKey string
	Region    string

	// QPS จำกัดฝั่งเรา ก่อนที่จะโดน Tencent จำกัดให้
	// CLB API มี limit ต่อ account/region ที่ต่ำกว่าที่คนส่วนใหญ่คาด
	QPS   float64
	Burst int

	TaskPollInterval time.Duration
	TaskTimeout      time.Duration
}

func (o *Options) defaults() {
	if o.QPS == 0 {
		o.QPS = 10
	}
	if o.Burst == 0 {
		o.Burst = 20
	}
	if o.TaskPollInterval == 0 {
		o.TaskPollInterval = 2 * time.Second
	}
	if o.TaskTimeout == 0 {
		o.TaskTimeout = 5 * time.Minute
	}
}

type client struct {
	clb     *sdk.Client
	limiter *rate.Limiter

	taskPollInterval time.Duration
	taskTimeout      time.Duration
}

// New สร้าง client ที่คุยกับ Tencent Cloud จริง
func New(opts Options) (Interface, error) {
	opts.defaults()
	if opts.Region == "" {
		return nil, fmt.Errorf("region is required")
	}

	cred := common.NewCredential(opts.SecretID, opts.SecretKey)
	cpf := profile.NewClientProfile()
	cpf.HttpProfile.ReqTimeout = 30
	cpf.Language = "en-US"

	c, err := sdk.NewClient(cred, opts.Region, cpf)
	if err != nil {
		return nil, fmt.Errorf("creating clb client: %w", err)
	}

	return &client{
		clb:              c,
		limiter:          rate.NewLimiter(rate.Limit(opts.QPS), opts.Burst),
		taskPollInterval: opts.TaskPollInterval,
		taskTimeout:      opts.TaskTimeout,
	}, nil
}

// ---------- load balancer ----------

func (c *client) FindByTags(ctx context.Context, tags map[string]string) ([]LoadBalancer, error) {
	req := sdk.NewDescribeLoadBalancersRequest()
	req.Forward = common.Int64Ptr(1) // 1 = CLB รุ่นใหม่ (application type) เท่านั้น
	req.Limit = common.Int64Ptr(100)

	// filter ชื่อ "tag:<key>" คือการกรองด้วยคู่ key/value ของ tag
	for k, v := range tags {
		req.Filters = append(req.Filters, &sdk.Filter{
			Name:   common.StringPtr("tag:" + k),
			Values: common.StringPtrs([]string{v}),
		})
	}

	var out []LoadBalancer
	var offset int64
	for {
		req.Offset = common.Int64Ptr(offset)

		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		resp, err := c.clb.DescribeLoadBalancersWithContext(ctx, req)
		if err != nil {
			return nil, classify(err)
		}
		if resp.Response == nil {
			break
		}
		for _, lb := range resp.Response.LoadBalancerSet {
			out = append(out, convertLB(lb))
		}
		total := deref(resp.Response.TotalCount)
		offset += 100
		if offset >= int64(total) || len(resp.Response.LoadBalancerSet) == 0 {
			break
		}
	}
	return out, nil
}

// Get คืน (nil, nil) เมื่อไม่พบ — "ถูกลบไปแล้ว" เป็นสถานะปกติที่ caller ต้องรับมือ
// ไม่ใช่ error
func (c *client) Get(ctx context.Context, id string) (*LoadBalancer, error) {
	req := sdk.NewDescribeLoadBalancersRequest()
	req.LoadBalancerIds = common.StringPtrs([]string{id})

	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := c.clb.DescribeLoadBalancersWithContext(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	if resp.Response == nil || len(resp.Response.LoadBalancerSet) == 0 {
		return nil, nil
	}
	lb := convertLB(resp.Response.LoadBalancerSet[0])
	return &lb, nil
}

func (c *client) Create(ctx context.Context, spec CreateSpec) (string, error) {
	req := sdk.NewCreateLoadBalancerRequest()
	req.LoadBalancerType = common.StringPtr(spec.Type)
	req.Forward = common.Int64Ptr(1)
	req.LoadBalancerName = common.StringPtr(spec.Name)
	req.Number = common.Uint64Ptr(1)

	if spec.VpcID != "" {
		req.VpcId = common.StringPtr(spec.VpcID)
	}
	// SubnetId ใช้เฉพาะ INTERNAL — ใส่ตอน OPEN จะโดนปฏิเสธ
	if spec.Type == LBTypeInternal && spec.SubnetID != "" {
		req.SubnetId = common.StringPtr(spec.SubnetID)
	}
	if spec.AddressIPVersion != "" {
		req.AddressIPVersion = common.StringPtr(spec.AddressIPVersion)
	}
	if spec.SLAType != "" {
		req.SlaType = common.StringPtr(spec.SLAType)
	}
	if spec.ClientToken != "" {
		req.ClientToken = common.StringPtr(spec.ClientToken)
	}
	// InternetAccessible ใช้กับ OPEN เท่านั้น
	if spec.Type == LBTypeOpen && (spec.ChargeType != "" || spec.BandwidthOut > 0) {
		ia := &sdk.InternetAccessible{}
		if spec.ChargeType != "" {
			ia.InternetChargeType = common.StringPtr(spec.ChargeType)
		}
		if spec.BandwidthOut > 0 {
			ia.InternetMaxBandwidthOut = common.Int64Ptr(spec.BandwidthOut)
		}
		req.InternetAccessible = ia
	}
	for k, v := range spec.Tags {
		req.Tags = append(req.Tags, &sdk.TagInfo{
			TagKey:   common.StringPtr(k),
			TagValue: common.StringPtr(v),
		})
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}
	resp, err := c.clb.CreateLoadBalancerWithContext(ctx, req)
	if err != nil {
		return "", classify(err)
	}
	if resp.Response == nil || len(resp.Response.LoadBalancerIds) == 0 {
		return "", fmt.Errorf("CreateLoadBalancer returned no load balancer id")
	}
	id := deref(resp.Response.LoadBalancerIds[0])

	// รอให้ CLB สร้างเสร็จจริงก่อน คืน id ให้ caller ไปเขียน annotation
	// ถ้า crash ระหว่างนี้ ClientToken เดิมจะทำให้ยิงซ้ำแล้วได้ตัวเดิม
	if err := c.waitTask(ctx, deref(resp.Response.RequestId)); err != nil {
		// สร้างไปแล้วแต่รอไม่สำเร็จ — คืน id ด้วยเพื่อให้ caller บันทึกไว้ก่อน
		// ไม่งั้นได้ CLB ผีที่ไม่มีใครเป็นเจ้าของ
		return id, err
	}
	return id, nil
}

func (c *client) Delete(ctx context.Context, id string) error {
	req := sdk.NewDeleteLoadBalancerRequest()
	req.LoadBalancerIds = common.StringPtrs([]string{id})

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.clb.DeleteLoadBalancerWithContext(ctx, req)
	if err != nil {
		return classify(err)
	}
	return c.waitTask(ctx, deref(resp.Response.RequestId))
}

// ---------- listener ----------

func (c *client) ListListeners(ctx context.Context, lbID string) ([]Listener, error) {
	req := sdk.NewDescribeListenersRequest()
	req.LoadBalancerId = common.StringPtr(lbID)

	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := c.clb.DescribeListenersWithContext(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	if resp.Response == nil {
		return nil, nil
	}
	out := make([]Listener, 0, len(resp.Response.Listeners))
	for _, l := range resp.Response.Listeners {
		out = append(out, convertListener(l))
	}
	return out, nil
}

func (c *client) CreateListener(ctx context.Context, lbID string, spec ListenerSpec) (string, error) {
	req := sdk.NewCreateListenerRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	req.Protocol = common.StringPtr(spec.Protocol)
	req.Ports = common.Int64Ptrs([]int64{spec.Port})
	req.ListenerNames = common.StringPtrs([]string{spec.Name})
	req.HealthCheck = toSDKHealthCheck(spec.HealthCheck)
	req.DeregisterTargetRst = common.BoolPtr(spec.DeregisterRT)

	if spec.Scheduler != "" {
		req.Scheduler = common.StringPtr(spec.Scheduler)
	}
	if spec.SessionTime > 0 {
		req.SessionExpireTime = common.Int64Ptr(spec.SessionTime)
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}
	resp, err := c.clb.CreateListenerWithContext(ctx, req)
	if err != nil {
		return "", classify(err)
	}
	if resp.Response == nil || len(resp.Response.ListenerIds) == 0 {
		return "", fmt.Errorf("CreateListener returned no listener id")
	}
	id := deref(resp.Response.ListenerIds[0])
	if err := c.waitTask(ctx, deref(resp.Response.RequestId)); err != nil {
		return id, err
	}
	return id, nil
}

func (c *client) UpdateListener(ctx context.Context, lbID, listenerID string, spec ListenerSpec) error {
	req := sdk.NewModifyListenerRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	req.ListenerId = common.StringPtr(listenerID)
	req.ListenerName = common.StringPtr(spec.Name)
	req.HealthCheck = toSDKHealthCheck(spec.HealthCheck)

	if spec.Scheduler != "" {
		req.Scheduler = common.StringPtr(spec.Scheduler)
	}
	if spec.SessionTime > 0 {
		req.SessionExpireTime = common.Int64Ptr(spec.SessionTime)
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.clb.ModifyListenerWithContext(ctx, req)
	if err != nil {
		return classify(err)
	}
	return c.waitTask(ctx, deref(resp.Response.RequestId))
}

func (c *client) DeleteListener(ctx context.Context, lbID, listenerID string) error {
	req := sdk.NewDeleteListenerRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	req.ListenerId = common.StringPtr(listenerID)

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.clb.DeleteListenerWithContext(ctx, req)
	if err != nil {
		return classify(err)
	}
	return c.waitTask(ctx, deref(resp.Response.RequestId))
}

// ---------- targets ----------

func (c *client) ListTargets(ctx context.Context, lbID string, listenerIDs []string) (map[string][]Target, error) {
	req := sdk.NewDescribeTargetsRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	if len(listenerIDs) > 0 {
		req.ListenerIds = common.StringPtrs(listenerIDs)
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := c.clb.DescribeTargetsWithContext(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	out := map[string][]Target{}
	if resp.Response == nil {
		return out, nil
	}
	for _, lb := range resp.Response.Listeners {
		id := deref(lb.ListenerId)
		targets := make([]Target, 0, len(lb.Targets))
		for _, t := range lb.Targets {
			targets = append(targets, Target{
				InstanceID: deref(t.InstanceId),
				Port:       deref(t.Port),
				Weight:     deref(t.Weight),
			})
		}
		out[id] = targets
	}
	return out, nil
}

func (c *client) RegisterTargets(ctx context.Context, lbID, listenerID string, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	req := sdk.NewRegisterTargetsRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	req.ListenerId = common.StringPtr(listenerID)
	req.Targets = toSDKTargets(targets)

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.clb.RegisterTargetsWithContext(ctx, req)
	if err != nil {
		return classify(err)
	}
	return c.waitTask(ctx, deref(resp.Response.RequestId))
}

func (c *client) DeregisterTargets(ctx context.Context, lbID, listenerID string, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	req := sdk.NewDeregisterTargetsRequest()
	req.LoadBalancerId = common.StringPtr(lbID)
	req.ListenerId = common.StringPtr(listenerID)
	req.Targets = toSDKTargets(targets)

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.clb.DeregisterTargetsWithContext(ctx, req)
	if err != nil {
		return classify(err)
	}
	return c.waitTask(ctx, deref(resp.Response.RequestId))
}

// ---------- conversion ----------

func convertLB(lb *sdk.LoadBalancer) LoadBalancer {
	out := LoadBalancer{
		ID:     deref(lb.LoadBalancerId),
		Name:   deref(lb.LoadBalancerName),
		Type:   deref(lb.LoadBalancerType),
		Domain: deref(lb.Domain),
		Status: deref(lb.Status),
		Tags:   map[string]string{},
	}
	for _, v := range lb.LoadBalancerVips {
		out.VIPs = append(out.VIPs, deref(v))
	}
	for _, t := range lb.Tags {
		out.Tags[deref(t.TagKey)] = deref(t.TagValue)
	}
	return out
}

func convertListener(l *sdk.Listener) Listener {
	out := Listener{
		ID:          deref(l.ListenerId),
		Name:        deref(l.ListenerName),
		Protocol:    deref(l.Protocol),
		Port:        deref(l.Port),
		Scheduler:   deref(l.Scheduler),
		SessionTime: deref(l.SessionExpireTime),
	}
	if hc := l.HealthCheck; hc != nil {
		out.HealthCheck = HealthCheck{
			Enabled:     deref(hc.HealthSwitch) == 1,
			Type:        deref(hc.CheckType),
			Port:        deref(hc.CheckPort),
			HTTPPath:    deref(hc.HttpCheckPath),
			HTTPDomain:  deref(hc.HttpCheckDomain),
			HTTPMethod:  deref(hc.HttpCheckMethod),
			Interval:    deref(hc.IntervalTime),
			Timeout:     deref(hc.TimeOut),
			HealthNum:   deref(hc.HealthNum),
			UnhealthNum: deref(hc.UnHealthNum),
		}
	}
	return out
}

func toSDKHealthCheck(hc HealthCheck) *sdk.HealthCheck {
	out := &sdk.HealthCheck{
		HealthSwitch: common.Int64Ptr(boolToInt64(hc.Enabled)),
	}
	if !hc.Enabled {
		return out
	}
	out.CheckType = common.StringPtr(hc.Type)
	if hc.Port > 0 {
		out.CheckPort = common.Int64Ptr(hc.Port)
	}
	if hc.Interval > 0 {
		out.IntervalTime = common.Int64Ptr(hc.Interval)
	}
	if hc.Timeout > 0 {
		out.TimeOut = common.Int64Ptr(hc.Timeout)
	}
	if hc.HealthNum > 0 {
		out.HealthNum = common.Int64Ptr(hc.HealthNum)
	}
	if hc.UnhealthNum > 0 {
		out.UnHealthNum = common.Int64Ptr(hc.UnhealthNum)
	}
	if hc.Type == HealthCheckHTTP {
		out.HttpCheckPath = common.StringPtr(hc.HTTPPath)
		out.HttpCheckMethod = common.StringPtr(hc.HTTPMethod)
		// ต้องส่งเสมอ ไม่งั้น CLB ปฏิเสธด้วย "HttpCheckDomain can't be None"
		out.HttpCheckDomain = common.StringPtr(hc.HTTPDomain)
		// HttpCode เป็น bitmask: 1=1xx 2=2xx 4=3xx 8=4xx 16=5xx
		// kube-proxy ตอบ 200 เมื่อ node มี local endpoint และ 503 เมื่อไม่มี
		// เอาแค่ 2xx จึงพอ และทำให้ node ที่ไม่มี pod ถูกถอนออกอัตโนมัติ
		out.HttpCode = common.Int64Ptr(2)
	}
	return out
}

func toSDKTargets(targets []Target) []*sdk.Target {
	out := make([]*sdk.Target, 0, len(targets))
	for _, t := range targets {
		out = append(out, &sdk.Target{
			InstanceId: common.StringPtr(t.InstanceID),
			Port:       common.Int64Ptr(t.Port),
			Weight:     common.Int64Ptr(t.Weight),
		})
	}
	return out
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
