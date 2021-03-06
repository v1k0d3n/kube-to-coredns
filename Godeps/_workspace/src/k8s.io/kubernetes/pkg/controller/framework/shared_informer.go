/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package framework

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/runtime"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
)

// if you use this, there is one behavior change compared to a standard Informer.
// When you receive a notification, the cache will be AT LEAST as fresh as the
// notification, but it MAY be more fresh.  You should NOT depend on the contents
// of the cache exactly matching the notification you've received in handler
// functions.  If there was a create, followed by a delete, the cache may NOT
// have your item.  This has advantages over the broadcaster since it allows us
// to share a common cache across many controllers. Extending the broadcaster
// would have required us keep duplicate caches for each watch.
type SharedInformer interface {
	// events to a single handler are delivered sequentially, but there is no coordination between different handlers
	// You may NOT add a handler *after* the SharedInformer is running.  That will result in an error being returned.
	// TODO we should try to remove this restriction eventually.
	AddEventHandler(handler ResourceEventHandler) error
	GetStore() cache.Store
	// GetController gives back a synthetic interface that "votes" to start the informer
	GetController() ControllerInterface
	Run(stopCh <-chan struct{})
	HasSynced() bool
}

type SharedIndexInformer interface {
	SharedInformer

	AddIndexer(indexer cache.Indexer) error
	GetIndexer() cache.Indexer
}

// NewSharedInformer creates a new instance for the listwatcher.
// TODO: create a cache/factory of these at a higher level for the list all, watch all of a given resource that can
// be shared amongst all consumers.
func NewSharedInformer(lw cache.ListerWatcher, objType runtime.Object, resyncPeriod time.Duration) SharedInformer {
	sharedInformer := &sharedInformer{
		processor: &sharedProcessor{},
		store:     cache.NewStore(DeletionHandlingMetaNamespaceKeyFunc),
	}

	fifo := cache.NewDeltaFIFO(cache.MetaNamespaceKeyFunc, nil, sharedInformer.store)

	cfg := &Config{
		Queue:            fifo,
		ListerWatcher:    lw,
		ObjectType:       objType,
		FullResyncPeriod: resyncPeriod,
		RetryOnError:     false,

		Process: sharedInformer.HandleDeltas,
	}
	sharedInformer.controller = New(cfg)

	return sharedInformer
}

type sharedInformer struct {
	store      cache.Store
	controller *Controller

	processor *sharedProcessor

	started     bool
	startedLock sync.Mutex
}

// dummyController hides the fact that a SharedInformer is different from a dedicated one
// where a caller can `Run`.  The run method is disonnected in this case, because higher
// level logic will decide when to start the SharedInformer and related controller.
// Because returning information back is always asynchronous, the legacy callers shouldn't
// notice any change in behavior.
type dummyController struct {
	informer *sharedInformer
}

func (v *dummyController) Run(stopCh <-chan struct{}) {
}

func (v *dummyController) HasSynced() bool {
	return v.informer.HasSynced()
}

type updateNotification struct {
	oldObj interface{}
	newObj interface{}
}

type addNotification struct {
	newObj interface{}
}

type deleteNotification struct {
	oldObj interface{}
}

func (s *sharedInformer) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		s.started = true
	}()

	s.processor.run(stopCh)
	s.controller.Run(stopCh)
}

func (s *sharedInformer) isStarted() bool {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
	return s.started
}

func (s *sharedInformer) HasSynced() bool {
	return s.controller.HasSynced()
}

func (s *sharedInformer) GetStore() cache.Store {
	return s.store
}

func (s *sharedInformer) GetController() ControllerInterface {
	return &dummyController{informer: s}
}

func (s *sharedInformer) AddEventHandler(handler ResourceEventHandler) error {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()

	if s.started {
		return fmt.Errorf("informer has already started")
	}

	listener := newProcessListener(handler)
	s.processor.listeners = append(s.processor.listeners, listener)
	return nil
}

func (s *sharedInformer) HandleDeltas(obj interface{}) error {
	// from oldest to newest
	for _, d := range obj.(cache.Deltas) {
		switch d.Type {
		case cache.Sync, cache.Added, cache.Updated:
			if old, exists, err := s.store.Get(d.Object); err == nil && exists {
				if err := s.store.Update(d.Object); err != nil {
					return err
				}
				s.processor.distribute(updateNotification{oldObj: old, newObj: d.Object})
			} else {
				if err := s.store.Add(d.Object); err != nil {
					return err
				}
				s.processor.distribute(addNotification{newObj: d.Object})
			}
		case cache.Deleted:
			if err := s.store.Delete(d.Object); err != nil {
				return err
			}
			s.processor.distribute(deleteNotification{oldObj: d.Object})
		}
	}
	return nil
}

type sharedProcessor struct {
	listeners []*processorListener
}

func (p *sharedProcessor) distribute(obj interface{}) {
	for _, listener := range p.listeners {
		listener.add(obj)
	}
}

func (p *sharedProcessor) run(stopCh <-chan struct{}) {
	for _, listener := range p.listeners {
		go listener.run(stopCh)
		go listener.pop(stopCh)
	}
}

type processorListener struct {
	// lock/cond protects access to 'pendingNotifications'.
	lock sync.RWMutex
	cond sync.Cond

	// pendingNotifications is an unbounded slice that holds all notifications not yet distributed
	// there is one per listener, but a failing/stalled listener will have infinite pendingNotifications
	// added until we OOM.
	// TODO This is no worse that before, since reflectors were backed by unbounded DeltaFIFOs, but
	// we should try to do something better
	pendingNotifications []interface{}

	nextCh chan interface{}

	handler ResourceEventHandler
}

func newProcessListener(handler ResourceEventHandler) *processorListener {
	ret := &processorListener{
		pendingNotifications: []interface{}{},
		nextCh:               make(chan interface{}),
		handler:              handler,
	}

	ret.cond.L = &ret.lock
	return ret
}

func (p *processorListener) add(notification interface{}) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.pendingNotifications = append(p.pendingNotifications, notification)
	p.cond.Broadcast()
}

func (p *processorListener) pop(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	p.lock.Lock()
	defer p.lock.Unlock()
	for {
		for len(p.pendingNotifications) == 0 {
			// check if we're shutdown
			select {
			case <-stopCh:
				return
			default:
			}

			p.cond.Wait()
		}
		notification := p.pendingNotifications[0]
		p.pendingNotifications = p.pendingNotifications[1:]

		select {
		case <-stopCh:
			return
		case p.nextCh <- notification:
		}
	}
}

func (p *processorListener) run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	for {
		var next interface{}
		select {
		case <-stopCh:
			func() {
				p.lock.Lock()
				defer p.lock.Unlock()
				p.cond.Broadcast()
			}()
			return
		case next = <-p.nextCh:
		}

		switch notification := next.(type) {
		case updateNotification:
			p.handler.OnUpdate(notification.oldObj, notification.newObj)
		case addNotification:
			p.handler.OnAdd(notification.newObj)
		case deleteNotification:
			p.handler.OnDelete(notification.oldObj)
		default:
			utilruntime.HandleError(fmt.Errorf("unrecognized notification: %#v", next))
		}
	}
}
