/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"fmt"
	"strings"
	"sync"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	clientv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	arbapi "github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/api"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/api/validation"
	arbv1 "github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/apis/v1"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/client"
	informerfactory "github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/client/informers"
	arbclient "github.com/kubernetes-incubator/kube-arbitrator/pkg/batchd/client/informers/v1"
)

// New returns a Cache implementation.
func New(config *rest.Config, schedulerName string) Cache {
	return newSchedulerCache(config, schedulerName)
}

type SchedulerCache struct {
	sync.Mutex

	podInformer            clientv1.PodInformer
	nodeInformer           clientv1.NodeInformer
	schedulingSpecInformer arbclient.SchedulingSpecInformer

	Tasks map[arbapi.TaskID]*arbapi.TaskInfo
	Jobs  map[arbapi.JobID]*arbapi.JobInfo
	Nodes map[string]*arbapi.NodeInfo
}

func newSchedulerCache(config *rest.Config, schedulerName string) *SchedulerCache {
	sc := &SchedulerCache{
		Jobs:  make(map[arbapi.JobID]*arbapi.JobInfo),
		Nodes: make(map[string]*arbapi.NodeInfo),
	}

	kubecli := kubernetes.NewForConfigOrDie(config)
	informerFactory := informers.NewSharedInformerFactory(kubecli, 0)

	// create informer for node information
	sc.nodeInformer = informerFactory.Core().V1().Nodes()
	sc.nodeInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    sc.AddNode,
			UpdateFunc: sc.UpdateNode,
			DeleteFunc: sc.DeleteNode,
		},
		0,
	)

	// create informer for pod information
	sc.podInformer = informerFactory.Core().V1().Pods()
	sc.podInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch obj.(type) {
				case *v1.Pod:
					pod := obj.(*v1.Pod)
					if strings.Compare(pod.Spec.SchedulerName, schedulerName) == 0 && pod.Status.Phase == v1.PodPending {
						return true
					}
					return pod.Status.Phase == v1.PodRunning
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    sc.AddPod,
				UpdateFunc: sc.UpdatePod,
				DeleteFunc: sc.DeletePod,
			},
		})

	// create queue informer
	queueClient, _, err := client.NewClient(config)
	if err != nil {
		panic(err)
	}

	schedulingSpecInformerFactory := informerfactory.NewSharedInformerFactory(queueClient, 0)
	// create informer for Queue information
	sc.schedulingSpecInformer = schedulingSpecInformerFactory.SchedulingSpec().SchedulingSpecs()
	sc.schedulingSpecInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *arbv1.SchedulingSpec:
					glog.V(4).Infof("Filter Queue name(%s) namespace(%s)\n", t.Name, t.Namespace)
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    sc.AddSchedulingSpec,
				UpdateFunc: sc.UpdateSchedulingSpec,
				DeleteFunc: sc.DeleteSchedulingSpec,
			},
		})

	return sc
}

func (sc *SchedulerCache) Run(stopCh <-chan struct{}) {
	go sc.podInformer.Informer().Run(stopCh)
	go sc.nodeInformer.Informer().Run(stopCh)
	go sc.schedulingSpecInformer.Informer().Run(stopCh)
}

func (sc *SchedulerCache) WaitForCacheSync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh,
		sc.podInformer.Informer().HasSynced,
		sc.schedulingSpecInformer.Informer().HasSynced,
		sc.nodeInformer.Informer().HasSynced)
}

// nonTerminatedPod selects pods that are non-terminal (pending and running).
func nonTerminatedPod(pod *v1.Pod) bool {
	if pod.Status.Phase == v1.PodSucceeded ||
		pod.Status.Phase == v1.PodFailed ||
		pod.Status.Phase == v1.PodUnknown {
		return false
	}
	return true
}

// UpdateStatus updates task status to the target status, return error if the transformation
// is invalid.
func (sc *SchedulerCache) UpdateStatus(task *arbapi.TaskInfo, status arbapi.TaskStatus) error {
	if err := validation.ValidateStatusUpdate(task.Status, status); err != nil {
		return err
	}

	task.Status = status

	return nil
}

func (sc *SchedulerCache) Snapshot() *arbapi.ClusterInfo {
	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	snapshot := &arbapi.ClusterInfo{
		Nodes: make([]*arbapi.NodeInfo, 0, len(sc.Nodes)),
		Jobs:  make([]*arbapi.JobInfo, 0, len(sc.Jobs)),
	}

	for _, value := range sc.Nodes {
		snapshot.Nodes = append(snapshot.Nodes, value.Clone())
	}

	for _, value := range sc.Jobs {
		snapshot.Jobs = append(snapshot.Jobs, value.Clone())
	}

	return snapshot
}

func (sc *SchedulerCache) String() string {
	sc.Mutex.Lock()
	defer sc.Mutex.Unlock()

	str := "Cache:\n"

	if len(sc.Nodes) != 0 {
		str = str + "Nodes:\n"
		for _, n := range sc.Nodes {
			str = str + fmt.Sprintf("\t %s: idle(%v) used(%v) allocatable(%v) pods(%d)\n",
				n.Name, n.Idle, n.Used, n.Allocatable, len(n.Tasks))

			i := 0
			for _, p := range n.Tasks {
				str = str + fmt.Sprintf("\t\t %d: %v\n", i, p)
				i++
			}
		}
	}

	if len(sc.Jobs) != 0 {
		str = str + "Jobs:\n"
		for _, job := range sc.Jobs {
			str = str + fmt.Sprintf("\t Job(%s) name(%s) minAvailable(%v)\n",
				job.UID, job.Name, job.MinAvailable)

			i := 0
			for _, tasks := range job.Tasks {
				for _, task := range tasks {
					str = str + fmt.Sprintf("\t\t %d: %v\n", i, task)
					i++
				}
			}
		}
	}

	return str
}
