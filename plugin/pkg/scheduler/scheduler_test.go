/*
Copyright 2014 The Kubernetes Authors.

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

package scheduler

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/testapi"
	clientcache "k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/diff"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"
	schedulertesting "k8s.io/kubernetes/plugin/pkg/scheduler/testing"
)

type fakeBinder struct {
	b func(binding *api.Binding) error
}

func (fb fakeBinder) Bind(binding *api.Binding) error { return fb.b(binding) }

type fakePodConditionUpdater struct{}

func (fc fakePodConditionUpdater) Update(pod *api.Pod, podCondition *api.PodCondition) error {
	return nil
}

func podWithID(id, desiredHost string) *api.Pod {
	return &api.Pod{
		ObjectMeta: api.ObjectMeta{Name: id, SelfLink: testapi.Default.SelfLink("pods", id)},
		Spec: api.PodSpec{
			NodeName: desiredHost,
		},
	}
}

func podWithPort(id, desiredHost string, port int) *api.Pod {
	pod := podWithID(id, desiredHost)
	pod.Spec.Containers = []api.Container{
		{Name: "ctr", Ports: []api.ContainerPort{{HostPort: int32(port)}}},
	}
	return pod
}

type mockScheduler struct {
	machine string
	err     error
}

func (es mockScheduler) Schedule(pod *api.Pod, ml algorithm.NodeLister) (string, error) {
	return es.machine, es.err
}

func TestScheduler(t *testing.T) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(t.Logf).Stop()
	errS := errors.New("scheduler")
	errB := errors.New("binder")

	table := []struct {
		injectBindError  error
		sendPod          *api.Pod
		algo             algorithm.ScheduleAlgorithm
		expectErrorPod   *api.Pod
		expectAssumedPod *api.Pod
		expectError      error
		expectBind       *api.Binding
		eventReason      string
	}{
		{
			sendPod:          podWithID("foo", ""),
			algo:             mockScheduler{"machine1", nil},
			expectBind:       &api.Binding{ObjectMeta: api.ObjectMeta{Name: "foo"}, Target: api.ObjectReference{Kind: "Node", Name: "machine1"}},
			expectAssumedPod: podWithID("foo", "machine1"),
			eventReason:      "Scheduled",
		}, {
			sendPod:        podWithID("foo", ""),
			algo:           mockScheduler{"machine1", errS},
			expectError:    errS,
			expectErrorPod: podWithID("foo", ""),
			eventReason:    "FailedScheduling",
		}, {
			sendPod:          podWithID("foo", ""),
			algo:             mockScheduler{"machine1", nil},
			expectBind:       &api.Binding{ObjectMeta: api.ObjectMeta{Name: "foo"}, Target: api.ObjectReference{Kind: "Node", Name: "machine1"}},
			expectAssumedPod: podWithID("foo", "machine1"),
			injectBindError:  errB,
			expectError:      errB,
			expectErrorPod:   podWithID("foo", ""),
			eventReason:      "FailedScheduling",
		},
	}

	for i, item := range table {
		var gotError error
		var gotPod *api.Pod
		var gotAssumedPod *api.Pod
		var gotBinding *api.Binding
		c := &Config{
			SchedulerCache: &schedulertesting.FakeCache{
				AssumeFunc: func(pod *api.Pod) {
					gotAssumedPod = pod
				},
			},
			NodeLister: algorithm.FakeNodeLister(
				api.NodeList{Items: []api.Node{{ObjectMeta: api.ObjectMeta{Name: "machine1"}}}},
			),
			Algorithm: item.algo,
			Binder: fakeBinder{func(b *api.Binding) error {
				gotBinding = b
				return item.injectBindError
			}},
			PodConditionUpdater: fakePodConditionUpdater{},
			Error: func(p *api.Pod, err error) {
				gotPod = p
				gotError = err
			},
			NextPod: func() *api.Pod {
				return item.sendPod
			},
			Recorder: eventBroadcaster.NewRecorder(api.EventSource{Component: "scheduler"}),
		}
		s := New(c)
		called := make(chan struct{})
		events := eventBroadcaster.StartEventWatcher(func(e *api.Event) {
			if e, a := item.eventReason, e.Reason; e != a {
				t.Errorf("%v: expected %v, got %v", i, e, a)
			}
			close(called)
		})
		s.scheduleOne()
		<-called
		if e, a := item.expectAssumedPod, gotAssumedPod; !reflect.DeepEqual(e, a) {
			t.Errorf("%v: assumed pod: wanted %v, got %v", i, e, a)
		}
		if e, a := item.expectErrorPod, gotPod; !reflect.DeepEqual(e, a) {
			t.Errorf("%v: error pod: wanted %v, got %v", i, e, a)
		}
		if e, a := item.expectError, gotError; !reflect.DeepEqual(e, a) {
			t.Errorf("%v: error: wanted %v, got %v", i, e, a)
		}
		if e, a := item.expectBind, gotBinding; !reflect.DeepEqual(e, a) {
			t.Errorf("%v: error: %s", i, diff.ObjectDiff(e, a))
		}
		events.Stop()
	}
}

func TestSchedulerNoPhantomPodAfterExpire(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	queuedPodStore := clientcache.NewFIFO(clientcache.MetaNamespaceKeyFunc)
	scache := schedulercache.New(100*time.Millisecond, stop)
	pod := podWithPort("pod.Name", "", 8080)
	scheduler, bindingChan, _ := setupTestSchedulerWithOnePod(t, queuedPodStore, scache, pod)

	waitPodExpireChan := make(chan struct{})
	timeout := make(chan struct{})
	go func() {
		for {
			select {
			case <-timeout:
				return
			default:
			}
			pods, err := scache.List(labels.Everything())
			if err != nil {
				t.Fatalf("cache.List failed: %v", err)
			}
			if len(pods) == 0 {
				close(waitPodExpireChan)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	// waiting for the assumed pod to expire
	select {
	case <-waitPodExpireChan:
	case <-time.After(wait.ForeverTestTimeout):
		close(timeout)
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	}

	// We use conflicted pod ports to incur fit predicate failure if first pod not removed.
	secondPod := podWithPort("bar", "", 8080)
	queuedPodStore.Add(secondPod)
	scheduler.scheduleOne()
	select {
	case b := <-bindingChan:
		expectBinding := &api.Binding{
			ObjectMeta: api.ObjectMeta{Name: "bar"},
			Target:     api.ObjectReference{Kind: "Node", Name: "machine1"},
		}
		if !reflect.DeepEqual(expectBinding, b) {
			t.Errorf("binding want=%v, get=%v", expectBinding, b)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	}
}

func TestSchedulerNoPhantomPodAfterDelete(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	queuedPodStore := clientcache.NewFIFO(clientcache.MetaNamespaceKeyFunc)
	scache := schedulercache.New(10*time.Minute, stop)
	firstPod := podWithPort("pod.Name", "", 8080)
	scheduler, bindingChan, errChan := setupTestSchedulerWithOnePod(t, queuedPodStore, scache, firstPod)

	// We use conflicted pod ports to incur fit predicate failure.
	secondPod := podWithPort("bar", "", 8080)
	queuedPodStore.Add(secondPod)
	// queuedPodStore: [bar:8080]
	// cache: [(assumed)foo:8080]

	scheduler.scheduleOne()
	select {
	case err := <-errChan:
		expectErr := &FitError{
			Pod:              secondPod,
			FailedPredicates: FailedPredicateMap{"machine1": "PodFitsHostPorts"},
		}
		if !reflect.DeepEqual(expectErr, err) {
			t.Errorf("err want=%v, get=%v", expectErr, err)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	}

	// We mimic the workflow of cache behavior when a pod is removed by user.
	// Note: if the schedulercache timeout would be super short, the first pod would expire
	// and would be removed itself (without any explicit actions on schedulercache). Even in that case,
	// explicitly AddPod will as well correct the behavior.
	firstPod.Spec.NodeName = "machine1"
	if err := scache.AddPod(firstPod); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := scache.RemovePod(firstPod); err != nil {
		t.Fatalf("err: %v", err)
	}

	queuedPodStore.Add(secondPod)
	scheduler.scheduleOne()
	select {
	case b := <-bindingChan:
		expectBinding := &api.Binding{
			ObjectMeta: api.ObjectMeta{Name: "bar"},
			Target:     api.ObjectReference{Kind: "Node", Name: "machine1"},
		}
		if !reflect.DeepEqual(expectBinding, b) {
			t.Errorf("binding want=%v, get=%v", expectBinding, b)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	}
}

// queuedPodStore: pods queued before processing.
// cache: scheduler cache that might contain assumed pods.
func setupTestSchedulerWithOnePod(t *testing.T, queuedPodStore *clientcache.FIFO, scache schedulercache.Cache, pod *api.Pod) (*Scheduler, chan *api.Binding, chan error) {
	// Create the scheduler config
	algo := NewGenericScheduler(
		scache,
		map[string]algorithm.FitPredicate{"PodFitsHostPorts": predicates.PodFitsHostPorts},
		[]algorithm.PriorityConfig{},
		[]algorithm.SchedulerExtender{})
	bindingChan := make(chan *api.Binding, 1)
	errChan := make(chan error, 1)
	cfg := &Config{
		SchedulerCache: scache,
		NodeLister: algorithm.FakeNodeLister(
			api.NodeList{Items: []api.Node{{ObjectMeta: api.ObjectMeta{Name: "machine1"}}}},
		),
		Algorithm: algo,
		Binder: fakeBinder{func(b *api.Binding) error {
			bindingChan <- b
			return nil
		}},
		NextPod: func() *api.Pod {
			return clientcache.Pop(queuedPodStore).(*api.Pod)
		},
		Error: func(p *api.Pod, err error) {
			errChan <- err
		},
		Recorder:            &record.FakeRecorder{},
		PodConditionUpdater: fakePodConditionUpdater{},
	}
	scheduler := New(cfg)

	queuedPodStore.Add(pod)
	// queuedPodStore: [foo:8080]
	// cache: []

	scheduler.scheduleOne()
	// queuedPodStore: []
	// cache: [(assumed)foo:8080]

	select {
	case b := <-bindingChan:
		expectBinding := &api.Binding{
			ObjectMeta: api.ObjectMeta{Name: "pod.Name"},
			Target:     api.ObjectReference{Kind: "Node", Name: "machine1"},
		}
		if !reflect.DeepEqual(expectBinding, b) {
			t.Errorf("binding want=%v, get=%v", expectBinding, b)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	}
	return scheduler, bindingChan, errChan
}
