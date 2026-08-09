package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SetupWithManager ต่อ reconciler เข้ากับ manager
//
// MaxConcurrentReconciles ตั้งได้อย่างปลอดภัยเพราะ workqueue key = Service key
// การันตีว่า CLB หนึ่งตัวถูกแตะโดย goroutine เดียวเสมอ ซึ่งจำเป็นเพราะ CLB
// ล็อกตัวเองระหว่างมี async task ค้าง
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(r.servicePredicate())).
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(endpointSliceToService),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToServices),
			builder.WithPredicates(nodeRelevantPredicate()),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Named("service-clb").
		Complete(r)
}

// servicePredicate กรอง Service ที่ไม่ใช่ของเราออกตั้งแต่ก่อนเข้า workqueue
func (r *ServiceReconciler) servicePredicate() predicate.Predicate {
	relevant := func(o client.Object) bool {
		svc, ok := o.(*corev1.Service)
		if !ok {
			return false
		}
		// Service ที่กำลังถูกลบต้องผ่านเสมอ ไม่งั้น finalizer ค้าง
		if svc.DeletionTimestamp != nil {
			return true
		}
		return r.owns(svc)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
		// เคยเป็นของเราแล้วถูกเปลี่ยน class ออก ก็ยังต้อง reconcile เพื่อเก็บกวาด
		UpdateFunc: func(e event.UpdateEvent) bool {
			return relevant(e.ObjectOld) || relevant(e.ObjectNew)
		},
	}
}

// endpointSliceToService map EndpointSlice กลับเป็น Service เจ้าของ
//
// นี่คือสิ่งที่ทำให้ externalTrafficPolicy: Local ตอบสนองเร็ว —
// pod ย้าย node แล้ว target เปลี่ยนทันที ไม่ต้องรอ health check timeout
func endpointSliceToService(_ context.Context, o client.Object) []reconcile.Request {
	name, ok := o.GetLabels()[discoveryv1.LabelServiceName]
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: serviceKey(o.GetNamespace(), name)}}
}

// nodeToServices enqueue ทุก Service ที่เราดูแล เมื่อ node มีการเปลี่ยนแปลงที่สำคัญ
func (r *ServiceReconciler) nodeToServices(ctx context.Context, _ client.Object) []reconcile.Request {
	var list corev1.ServiceList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		svc := &list.Items[i]
		if !r.owns(svc) {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: serviceKey(svc.Namespace, svc.Name)})
	}
	return reqs
}

// nodeRelevantPredicate กรอง node event ที่ไม่กระทบ target pool ออก
//
// node object ถูกอัปเดตบ่อยมาก (heartbeat, resource usage) — ถ้าไม่กรอง
// เราจะ reconcile ทุก Service ทุกไม่กี่วินาที และเผา CLB API quota ทิ้ง
func nodeRelevantPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}
			if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable {
				return true
			}
			if nodeReady(oldNode) != nodeReady(newNode) {
				return true
			}
			return !sameTaints(oldNode, newNode)
		},
	}
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func sameTaints(a, b *corev1.Node) bool {
	if len(a.Spec.Taints) != len(b.Spec.Taints) {
		return false
	}
	seen := make(map[string]bool, len(a.Spec.Taints))
	for _, t := range a.Spec.Taints {
		seen[t.Key+"="+t.Value+":"+string(t.Effect)] = true
	}
	for _, t := range b.Spec.Taints {
		if !seen[t.Key+"="+t.Value+":"+string(t.Effect)] {
			return false
		}
	}
	return true
}
