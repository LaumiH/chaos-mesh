// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package containerkill

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/go-logr/logr"

	"github.com/pingcap/chaos-mesh/api/v1alpha1"
	pb "github.com/pingcap/chaos-mesh/pkg/chaosdaemon/pb"
	"github.com/pingcap/chaos-mesh/pkg/utils"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	containerKillActionMsg = "delete container %s"
)

type Reconciler struct {
	client.Client
	Log logr.Logger
}

func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var err error
	now := time.Now()

	r.Log.Info("reconciling container kill")
	ctx := context.Background()

	var podchaos v1alpha1.PodChaos
	if err = r.Get(ctx, req.NamespacedName, &podchaos); err != nil {
		r.Log.Error(err, "unable to get podchaos")
		return ctrl.Result{}, nil
	}

	if podchaos.Spec.ContainerName == "" {
		r.Log.Error(nil, "the name of container is empty", "name", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	shouldAct := podchaos.GetNextStart().Before(now)
	if !shouldAct {
		return ctrl.Result{RequeueAfter: podchaos.GetNextStart().Sub(now)}, nil
	}
	pods, err := utils.SelectPods(ctx, r.Client, podchaos.Spec.Selector)
	if err != nil {
		r.Log.Error(err, "fail to get selected pods")
		return ctrl.Result{Requeue: true}, nil
	}

	if len(pods) == 0 {
		r.Log.Error(nil, "no pod is selected", "name", req.Name, "namespace", req.Namespace)
		return ctrl.Result{Requeue: true}, nil
	}

	filteredPod, err := utils.GeneratePods(pods, podchaos.Spec.Mode, podchaos.Spec.Value)
	if err != nil {
		r.Log.Error(err, "fail to generate pods")
		return ctrl.Result{Requeue: true}, nil
	}

	g := errgroup.Group{}
	for podIndex := range filteredPod {
		pod := &filteredPod[podIndex]
		haveContainer := false

		for containerIndex := range pod.Status.ContainerStatuses {
			containerName := pod.Status.ContainerStatuses[containerIndex].Name
			containerID := pod.Status.ContainerStatuses[containerIndex].ContainerID

			if containerName == podchaos.Spec.ContainerName {
				haveContainer = true
				err = r.KillContainer(ctx, pod, containerID)
				if err != nil {
					r.Log.Error(err, "failed to kill container")
				}
			}
		}

		if haveContainer == false {
			r.Log.Error(nil, fmt.Sprintf("the pod %s doesn't have container %s", pod.Name, podchaos.Spec.ContainerName))
		}
	}

	if err := g.Wait(); err != nil {
		return ctrl.Result{}, nil
	}

	return r.updatePodchaos(ctx, podchaos, pods, now)
}

// KillContainer kills container according to containerID
// Use client in chaos-daemon
func (r *Reconciler) KillContainer(ctx context.Context, pod *v1.Pod, containerID string) error {
	r.Log.Info("try to kill container", "namespace", pod.Namespace, "podName", pod.Name, "containerID", containerID)

	c, err := utils.CreateGrpcConnection(ctx, r.Client, pod)
	if err != nil {
		return err
	}
	defer c.Close()

	pbClient := pb.NewChaosDaemonClient(c)

	if len(pod.Status.ContainerStatuses) == 0 {
		return fmt.Errorf("%s %s can't get the state of container", pod.Namespace, pod.Name)
	}

	if _, err = pbClient.ContainerKill(ctx, &pb.ContainerRequest{
		Action: &pb.ContainerAction{
			Action: pb.ContainerAction_KILL,
		},
		ContainerId: containerID,
	}); err != nil {
		r.Log.Error(err, "kill container error", "namespace", pod.Namespace, "podName", pod.Name, "containerID", containerID)
		return err
	}

	return nil
}

func (r *Reconciler) updatePodchaos(ctx context.Context, podchaos v1alpha1.PodChaos, pods []v1.Pod, now time.Time) (ctrl.Result, error) {
	next, err := utils.NextTime(*podchaos.Spec.Scheduler, now)
	if err != nil {
		r.Log.Error(err, "failed to get next time")
		return ctrl.Result{}, nil
	}

	podchaos.SetNextStart(*next)

	podchaos.Status.Experiment.StartTime = &metav1.Time{
		Time: now,
	}
	podchaos.Status.Experiment.EndTime = &metav1.Time{
		Time: now,
	}

	podchaos.Status.Experiment.Pods = []v1alpha1.PodStatus{}
	for _, pod := range pods {
		ps := v1alpha1.PodStatus{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			HostIP:    pod.Status.HostIP,
			PodIP:     pod.Status.PodIP,
			Action:    string(podchaos.Spec.Action),
			Message:   fmt.Sprintf(containerKillActionMsg, podchaos.Spec.ContainerName),
		}

		podchaos.Status.Experiment.Pods = append(podchaos.Status.Experiment.Pods, ps)
	}
	if err := r.Update(ctx, &podchaos); err != nil {
		r.Log.Error(err, "unable to update chaosctl status")
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}
