package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/config"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/node"
)

// ServiceReconciler ทำให้ CLB ตรงกับ Service type=LoadBalancer หนึ่งตัว
//
// สัญญาสำคัญ: workqueue key = Service key และ 1 Service = 1 CLB
// จึงได้การ serialize ต่อ CLB มาฟรี ซึ่งจำเป็นมาก เพราะ CLB ล็อกตัวเอง
// ระหว่างมี async task ค้าง — ยิงสองคำสั่งพร้อมกันบนตัวเดียวกันจะ fail
type ServiceReconciler struct {
	client.Client
	CLB      clb.Interface
	Nodes    node.Resolver
	Recorder record.EventRecorder
	Config   *config.Config
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		// Service หายไปแล้ว: finalizer การันตีว่า cleanup เกิดก่อนหน้านี้แล้ว
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !r.owns(&svc) {
		return ctrl.Result{}, nil
	}

	if svc.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, &svc)
	}
	return r.reconcileNormal(ctx, &svc, logger)
}

// owns บอกว่า Service นี้เป็นความรับผิดชอบของ controller ตัวนี้หรือไม่
//
// ใช้ spec.loadBalancerClass ไม่ใช่ annotation เพราะเป็นกลไกมาตรฐานของ k8s
// และเป็นสิ่งเดียวกับที่ทำให้ ServiceLB (klipper) ของ K3s ยอมปล่อย Service นี้ไป
func (r *ServiceReconciler) owns(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	if svc.Spec.LoadBalancerClass == nil {
		return false
	}
	c := *svc.Spec.LoadBalancerClass
	return c == r.Config.ClassExternal || c == r.Config.ClassInternal
}

func (r *ServiceReconciler) reconcileNormal(ctx context.Context, svc *corev1.Service, logger logr.Logger) (ctrl.Result, error) {
	spec, err := config.Parse(svc, r.Config)
	if err != nil {
		// annotation ผิด → บอกให้ชัด แล้วรอคนแก้ ไม่ต้อง retry รัว
		r.event(svc, corev1.EventTypeWarning, "InvalidConfiguration", err.Error())
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// ใส่ finalizer ก่อนแตะ cloud เสมอ
	// ถ้า Service ถูกลบหลังจากนี้ เรายังมีโอกาสเก็บกวาด CLB
	if controllerutil.AddFinalizer(svc, config.Finalizer) {
		if err := r.Update(ctx, svc); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	r.event(svc, corev1.EventTypeNormal, "EnsuringLoadBalancer", "Ensuring CLB")

	lb, err := r.ensureLoadBalancer(ctx, svc, spec)
	if err != nil {
		r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
		return r.retry(err)
	}
	if !lb.Ready() {
		// CLB ยังสร้างไม่เสร็จ — สร้าง listener ตอนนี้จะ fail
		logger.Info("load balancer not ready yet", "id", lb.ID, "status", lb.Status)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if lb.DeleteProtect != spec.DeleteProtect {
		logger.Info("updating delete protection", "id", lb.ID, "enabled", spec.DeleteProtect)
		if err := r.CLB.SetDeleteProtection(ctx, lb.ID, spec.DeleteProtect); err != nil {
			r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
			return r.retry(err)
		}
		lb.DeleteProtect = spec.DeleteProtect
	}

	// ลำดับสำคัญ: ผูก SG ให้เสร็จก่อนแล้วค่อยเปิด pass-to-target
	// สลับลำดับแล้วจะมีช่วงที่ SG ของ node ถูกข้ามไปแล้วแต่ SG ของ CLB ยังไม่มา
	// ซึ่งคือช่วงที่ backend เปิดรับ traffic จากทุกที่
	if spec.SecurityGroups != nil && !sameStrings(lb.SecurityGroups, spec.SecurityGroups) {
		logger.Info("updating security groups", "id", lb.ID, "groups", spec.SecurityGroups)
		if err := r.CLB.SetSecurityGroups(ctx, lb.ID, spec.SecurityGroups); err != nil {
			r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
			return r.retry(err)
		}
		lb.SecurityGroups = spec.SecurityGroups
	}

	if spec.PassToTarget != nil && lb.PassToTarget != *spec.PassToTarget {
		logger.Info("updating pass-to-target", "id", lb.ID, "enabled", *spec.PassToTarget)
		if *spec.PassToTarget && len(lb.SecurityGroups) == 0 {
			// ไม่ใช่ error — เป็นการตั้งค่าที่ใช้ได้จริงและเป็นทางที่คนเลือกบ่อย
			// แต่ต้องรู้ตัวว่าไม่เหลือ SG ชั้นไหนกรอง backend อีกแล้ว
			r.event(svc, corev1.EventTypeWarning, "BackendSecurityGroupBypassed",
				"pass-to-target is on and no security group is bound to CLB "+lb.ID+
					": backend node ports accept traffic from anywhere the CLB can reach")
		}
		if err := r.CLB.SetPassToTarget(ctx, lb.ID, *spec.PassToTarget); err != nil {
			r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
			return r.retry(err)
		}
		lb.PassToTarget = *spec.PassToTarget
	}

	listeners, err := r.reconcileListeners(ctx, svc, lb.ID, spec)
	if err != nil {
		r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
		return r.retry(err)
	}

	if err := r.reconcileTargets(ctx, svc, lb.ID, listeners); err != nil {
		r.event(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", err.Error())
		return r.retry(err)
	}

	// ไม่มีทั้ง IP และ domain = ยังเรียกใช้ไม่ได้จริง
	// อย่าประกาศว่าเสร็จ ไม่งั้น Service ค้าง <pending> พร้อม Event ที่บอกว่าสำเร็จ
	// ซึ่งเป็นสถานะที่ debug ยากที่สุด
	if !lb.HasAddress() {
		r.event(svc, corev1.EventTypeWarning, "LoadBalancerAddressPending",
			"CLB "+lb.ID+" has neither a VIP nor a domain yet")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := r.updateStatus(ctx, svc, lb); err != nil {
		return ctrl.Result{}, err
	}

	r.event(svc, corev1.EventTypeNormal, "EnsuredLoadBalancer", "CLB "+lb.ID+" is in sync")
	// resync เป็นรอบเพื่อแก้ drift ที่เกิดนอก k8s (คนไปแก้บน console)
	return ctrl.Result{RequeueAfter: r.Config.ResyncPeriod}, nil
}

// ensureLoadBalancer หา CLB ที่ควรใช้ หรือสร้างใหม่
//
// ลำดับสำคัญมาก และออกแบบให้รอดจากการ crash ทุกจุด:
//  1. annotation existing-loadbalancer-id → user เอามาให้ใช้ ไม่เคยลบ
//  2. annotation loadbalancer-id → ที่เราเคยสร้าง
//  3. หาจาก tag → กู้กรณี crash หลังสร้างเสร็จแต่ก่อนเขียน annotation
//  4. สร้างใหม่ (มี ClientToken กัน duplicate ซ้อนอีกชั้น)
func (r *ServiceReconciler) ensureLoadBalancer(ctx context.Context, svc *corev1.Service, spec config.LBSpec) (*clb.LoadBalancer, error) {
	logger := log.FromContext(ctx)

	if spec.Adopted() {
		lb, err := r.CLB.Get(ctx, spec.ExistingID)
		if err != nil {
			return nil, fmt.Errorf("getting adopted load balancer %s: %w", spec.ExistingID, err)
		}
		if lb == nil {
			return nil, fmt.Errorf("adopted load balancer %s does not exist", spec.ExistingID)
		}
		return lb, nil
	}

	if id := svc.Annotations[config.AnnoLoadBalancerID]; id != "" {
		lb, err := r.CLB.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("getting load balancer %s: %w", id, err)
		}
		if lb != nil {
			return lb, nil
		}
		// annotation ชี้ไปยัง CLB ที่ถูกลบไปแล้ว — ล้างทิ้งแล้วสร้างใหม่
		logger.Info("load balancer in annotation no longer exists, recreating", "id", id)
	}

	// กู้จาก tag: ครอบเคส "สร้าง CLB สำเร็จแล้ว crash ก่อนเขียน annotation"
	// ถ้าไม่มีขั้นนี้ เราจะสร้าง CLB ใหม่ทุกครั้งที่ crash → CLB ผีที่คิดเงินไปเรื่อยๆ
	tags := config.OwnershipTags(svc, r.Config.ClusterID)
	found, err := r.CLB.FindByTags(ctx, tags)
	if err != nil {
		return nil, fmt.Errorf("searching load balancer by tags: %w", err)
	}
	if len(found) > 1 {
		// เจอหลายตัวแปลว่ามีอะไรผิดปกติจริง — หยุดดีกว่าเดาแล้วลบผิดตัว
		return nil, &clb.TerminalError{Err: fmt.Errorf(
			"found %d load balancers tagged for service %s/%s; resolve manually",
			len(found), svc.Namespace, svc.Name)}
	}
	if len(found) == 1 {
		lb := found[0]
		logger.Info("adopted load balancer found by tags", "id", lb.ID)
		if err := r.patchLBID(ctx, svc, lb.ID); err != nil {
			return nil, err
		}
		return &lb, nil
	}

	logger.Info("creating load balancer", "name", spec.Create.Name, "type", spec.Create.Type)
	id, createErr := r.CLB.Create(ctx, spec.Create)

	// บันทึก id ทันทีที่รู้ แม้ Create จะ error ระหว่างรอ task
	// ลำดับนี้จงใจ: ปล่อยให้ annotation หายแปลว่าอาจเหลือ CLB ที่ไม่มีเจ้าของ
	if id != "" {
		if err := r.patchLBID(ctx, svc, id); err != nil {
			return nil, err
		}
	}
	if createErr != nil {
		return nil, fmt.Errorf("creating load balancer: %w", createErr)
	}

	lb, err := r.CLB.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting created load balancer %s: %w", id, err)
	}
	if lb == nil {
		return nil, fmt.Errorf("load balancer %s disappeared right after creation", id)
	}
	r.event(svc, corev1.EventTypeNormal, "CreatedLoadBalancer", "Created CLB "+id)
	return lb, nil
}

// reconcileListeners ทำให้ listener บน CLB ตรงกับ ServicePort
// คืน mapping จาก listener id → nodePort เพื่อใช้ต่อในขั้น target
func (r *ServiceReconciler) reconcileListeners(ctx context.Context, svc *corev1.Service, lbID string, spec config.LBSpec) (map[string]int64, error) {
	logger := log.FromContext(ctx)

	actual, err := r.CLB.ListListeners(ctx, lbID)
	if err != nil {
		return nil, fmt.Errorf("listing listeners: %w", err)
	}

	// CLB ที่ user เอามาให้ (BYO) อาจมี listener ของคนอื่นอยู่ ห้ามลบ
	var owned map[clb.ListenerKey]bool
	if spec.Adopted() {
		owned = map[clb.ListenerKey]bool{}
		for _, l := range spec.Listeners {
			owned[l.Key()] = true
		}
	}

	plan := clb.PlanListeners(spec.Listeners, actual, owned)

	// nodePort ของแต่ละ listener หาได้จาก health check ของ TCP
	// แต่แหล่งที่เชื่อถือได้จริงคือ ServicePort — map ตาม port ที่ CLB เปิดรับ
	nodePortByPort := map[int64]int64{}
	for _, p := range svc.Spec.Ports {
		nodePortByPort[int64(p.Port)] = int64(p.NodePort)
	}

	out := map[string]int64{}

	for _, l := range plan.Delete {
		logger.Info("deleting listener", "listener", l.ID, "port", l.Port)
		if err := r.CLB.DeleteListener(ctx, lbID, l.ID); err != nil {
			return nil, fmt.Errorf("deleting listener %s: %w", l.ID, err)
		}
	}
	for _, u := range plan.Update {
		logger.Info("updating listener", "listener", u.Existing.ID, "port", u.Desired.Port)
		if err := r.CLB.UpdateListener(ctx, lbID, u.Existing.ID, u.Desired); err != nil {
			return nil, fmt.Errorf("updating listener %s: %w", u.Existing.ID, err)
		}
		out[u.Existing.ID] = nodePortByPort[u.Desired.Port]
	}
	for _, s := range plan.Create {
		logger.Info("creating listener", "protocol", s.Protocol, "port", s.Port)
		id, err := r.CLB.CreateListener(ctx, lbID, s)
		if err != nil {
			return nil, fmt.Errorf("creating listener %s/%d: %w", s.Protocol, s.Port, err)
		}
		out[id] = nodePortByPort[s.Port]
	}
	for _, m := range plan.Unchanged {
		out[m.Existing.ID] = nodePortByPort[m.Desired.Port]
	}

	return out, nil
}

// reconcileTargets ทำให้ backend ของทุก listener ตรงกับ node ที่ควรรับ traffic
func (r *ServiceReconciler) reconcileTargets(ctx context.Context, svc *corev1.Service, lbID string, listeners map[string]int64) error {
	logger := log.FromContext(ctx)
	if len(listeners) == 0 {
		return nil
	}

	nodes, err := r.eligibleNodes(ctx, svc)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(listeners))
	for id := range listeners {
		ids = append(ids, id)
	}
	actual, err := r.CLB.ListTargets(ctx, lbID, ids)
	if err != nil {
		return fmt.Errorf("listing targets: %w", err)
	}

	for listenerID, nodePort := range listeners {
		if nodePort == 0 {
			continue
		}
		desired, unresolved := r.buildTargets(ctx, nodes, nodePort)

		// ถ้า resolve ไม่ได้เลยสักตัวทั้งที่มี node ให้ใช้ — อย่าถอน backend ทิ้งทั้งหมด
		// การเข้าใจผิดว่า "ไม่มี node" แล้ว deregister หมด = ตัด traffic ทั้ง service
		if len(desired) == 0 && len(nodes) > 0 {
			r.event(svc, corev1.EventTypeWarning, "NodesNotResolvable",
				fmt.Sprintf("cannot resolve any of %d nodes to CVM instances; keeping existing targets", len(nodes)))
			return fmt.Errorf("no nodes resolvable to CVM instances")
		}
		if unresolved > 0 {
			r.event(svc, corev1.EventTypeWarning, "NodesNotResolvable",
				fmt.Sprintf("%d node(s) skipped: no matching CVM instance", unresolved))
		}

		plan := clb.PlanTargets(desired, actual[listenerID])
		if plan.Empty() {
			continue
		}

		// register ก่อน deregister เสมอ — ไม่งั้นมีช่วงที่ backend ว่างและ traffic ตก
		if err := r.CLB.RegisterTargets(ctx, lbID, listenerID, plan.Register); err != nil {
			return fmt.Errorf("registering targets on %s: %w", listenerID, err)
		}
		if err := r.CLB.DeregisterTargets(ctx, lbID, listenerID, plan.Deregister); err != nil {
			return fmt.Errorf("deregistering targets on %s: %w", listenerID, err)
		}
		logger.Info("targets synced", "listener", listenerID,
			"registered", len(plan.Register), "deregistered", len(plan.Deregister))
	}
	return nil
}

// updateStatus เขียนที่อยู่ของ CLB ลง status.loadBalancer.ingress
//
// บางภูมิภาค (เช่น ap-bangkok) ให้ CLB เป็นแบบ DNS — LoadBalancerVips จะว่าง
// และต้องใช้ Domain แทน ซึ่งตรงกับ field Hostname ของ Kubernetes
// การอ่านแต่ VIP อย่างเดียวทำให้ Service ค้าง <pending> ตลอดกาล
func (r *ServiceReconciler) updateStatus(ctx context.Context, svc *corev1.Service, lb *clb.LoadBalancer) error {
	var ingress corev1.LoadBalancerIngress
	if vip := lb.VIP(); vip != "" {
		ingress.IP = vip
	} else {
		ingress.Hostname = lb.Domain
	}

	cur := svc.Status.LoadBalancer.Ingress
	if len(cur) == 1 && cur[0].IP == ingress.IP && cur[0].Hostname == ingress.Hostname {
		return nil
	}

	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{ingress}
	if err := r.Status().Patch(ctx, svc, patch); err != nil {
		return fmt.Errorf("updating service status: %w", err)
	}
	return nil
}

// reconcileDelete เก็บกวาด CLB แล้วค่อยปล่อย Service ให้ถูกลบ
func (r *ServiceReconciler) reconcileDelete(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(svc, config.Finalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.cleanup(ctx, svc); err != nil {
		r.event(svc, corev1.EventTypeWarning, "DeleteLoadBalancerFailed", err.Error())
		return r.retry(err)
	}

	logger.Info("cleanup complete, releasing finalizer")
	controllerutil.RemoveFinalizer(svc, config.Finalizer)
	if err := r.Update(ctx, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) cleanup(ctx context.Context, svc *corev1.Service) error {
	logger := log.FromContext(ctx)

	// CLB ที่ user เอามาให้ใช้: ถอน listener ที่เราสร้าง แต่ไม่ลบตัว CLB
	if id := svc.Annotations[config.AnnoExistingLoadBalancerID]; id != "" {
		logger.Info("adopted load balancer, leaving it in place", "id", id)
		return r.cleanupAdopted(ctx, svc, id)
	}

	id := svc.Annotations[config.AnnoLoadBalancerID]
	if id == "" {
		// annotation หาย — หาจาก tag ก่อนยอมแพ้ ไม่งั้นเหลือ CLB ที่คิดเงินต่อไป
		found, err := r.CLB.FindByTags(ctx, config.OwnershipTags(svc, r.Config.ClusterID))
		if err != nil {
			return fmt.Errorf("searching load balancer by tags during cleanup: %w", err)
		}
		if len(found) == 0 {
			return nil
		}
		id = found[0].ID
		logger.Info("found orphaned load balancer by tags during cleanup", "id", id)
	}

	lb, err := r.CLB.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("getting load balancer %s: %w", id, err)
	}
	if lb == nil {
		return nil // ถูกลบไปแล้ว
	}

	// delete protection กันไม่ให้ลบได้ รวมถึงจาก controller เอง
	//
	// ปิดให้อัตโนมัติแทนที่จะหยุด เพราะถ้าหยุด finalizer จะค้างและ Service
	// จะติด Terminating ตลอดกาล เจตนาของ protection คือกันคนพลาดจาก console
	// ส่วน Service ใน Kubernetes เป็น source of truth — ลบ Service = สั่งให้ CLB หายไป
	// บอกด้วย Event ให้เห็นชัดว่าเกิดอะไรขึ้น ไม่ทำเงียบๆ
	if lb.DeleteProtect {
		logger.Info("clearing delete protection before deletion", "id", id)
		r.event(svc, corev1.EventTypeWarning, "ClearingDeleteProtection",
			"Disabling delete protection on CLB "+id+" because its Service was deleted")
		if err := r.CLB.SetDeleteProtection(ctx, id, false); err != nil {
			return fmt.Errorf("clearing delete protection on %s: %w", id, err)
		}
	}

	logger.Info("deleting load balancer", "id", id)
	if err := r.CLB.Delete(ctx, id); err != nil {
		return fmt.Errorf("deleting load balancer %s: %w", id, err)
	}
	r.event(svc, corev1.EventTypeNormal, "DeletedLoadBalancer", "Deleted CLB "+id)
	return nil
}

func (r *ServiceReconciler) cleanupAdopted(ctx context.Context, svc *corev1.Service, lbID string) error {
	spec, err := config.Parse(svc, r.Config)
	if err != nil {
		// parse ไม่ได้ตอนลบ — ยอมปล่อยดีกว่าค้าง Terminating ตลอดกาล
		return nil
	}
	actual, err := r.CLB.ListListeners(ctx, lbID)
	if err != nil {
		return fmt.Errorf("listing listeners: %w", err)
	}
	wanted := map[clb.ListenerKey]bool{}
	for _, l := range spec.Listeners {
		wanted[l.Key()] = true
	}
	for _, l := range actual {
		if !wanted[l.Key()] {
			continue
		}
		if err := r.CLB.DeleteListener(ctx, lbID, l.ID); err != nil {
			return fmt.Errorf("deleting listener %s: %w", l.ID, err)
		}
	}
	return nil
}

// patchLBID เขียน id ของ CLB กลับลง Service ทันทีที่รู้
// ใช้ patch ไม่ใช่ update เพื่อไม่ชนกับคนอื่นที่แก้ Service อยู่
func (r *ServiceReconciler) patchLBID(ctx context.Context, svc *corev1.Service, id string) error {
	if svc.Annotations[config.AnnoLoadBalancerID] == id {
		return nil
	}
	patch := client.MergeFrom(svc.DeepCopy())
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[config.AnnoLoadBalancerID] = id
	if err := r.Patch(ctx, svc, patch); err != nil {
		return fmt.Errorf("recording load balancer id on service: %w", err)
	}
	return nil
}

// retry เลือก backoff ตามชนิดของ error
//
// terminal error (auth พัง, parameter ผิด, quota เต็ม) retry ไปก็ไม่หาย
// การ retry รัวจะเผา API quota และกลบ error จริงใน log จนหาไม่เจอ
func (r *ServiceReconciler) retry(err error) (ctrl.Result, error) {
	if clb.IsTerminal(err) {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{}, err
}

func (r *ServiceReconciler) event(svc *corev1.Service, eventType, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(svc, eventType, reason, msg)
}

// serviceKey ใช้ map จาก object อื่นกลับมาเป็น Service
func serviceKey(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

// sameStrings เทียบแบบสนใจลำดับ
//
// ลำดับของ security group บน CLB คือลำดับความสำคัญของ rule ไม่ใช่แค่ set
// สลับที่กันแล้วผลลัพธ์ต่างกันจริง จึงต้องถือว่าเป็นคนละค่า
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
