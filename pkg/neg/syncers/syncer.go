/*
Copyright 2018 The Kubernetes Authors.

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

package syncers

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// syncer is a NEG syncer skeleton.
// It handles state transitions and backoff retry operations.
type syncer struct {
	// metadata
	NegSyncerKey
	negName string

	// NEG sync function
	syncFunc func() error

	// event recording
	serviceLister cache.Indexer
	recorder      record.EventRecorder

	// syncer states
	stateLock    sync.Mutex
	stopped      bool
	shuttingDown bool

	// sync signal and retry handling
	syncCh  chan interface{}
	clock   clock.Clock
	backoff backoffHandler
}

func newSyncer(negSyncerKey NegSyncerKey, networkEndpointGroupName string, serviceLister cache.Indexer, recorder record.EventRecorder) *syncer {
	return &syncer{
		NegSyncerKey:  negSyncerKey,
		negName:       networkEndpointGroupName,
		syncFunc:      func() error { return nil },
		serviceLister: serviceLister,
		recorder:      recorder,
		stopped:       true,
		shuttingDown:  false,
		clock:         clock.RealClock{},
		backoff:       NewExponentialBackendOffHandler(maxRetries, minRetryDelay, maxRetryDelay),
	}
}

func (s *syncer) Start() error {
	if !s.IsStopped() {
		return fmt.Errorf("NEG syncer for %s is already running.", s.NegSyncerKey.String())
	}
	if s.IsShuttingDown() {
		return fmt.Errorf("NEG syncer for %s is shutting down. ", s.NegSyncerKey.String())
	}

	glog.V(2).Infof("Starting NEG syncer for service port %s", s.NegSyncerKey.String())
	s.init()
	go func() {
		for {
			// equivalent to never retry
			retryCh := make(<-chan time.Time)
			err := s.syncFunc()
			if err != nil {
				delay, retryErr := s.backoff.NextRetryDelay()
				retryMesg := ""
				if retryErr == ErrRetriesExceeded {
					retryMesg = "(will not retry)"
				} else {
					retryCh = s.clock.After(delay)
					retryMesg = "(will retry)"
				}

				if svc := getService(s.serviceLister, s.Namespace, s.Name); svc != nil {
					s.recorder.Eventf(svc, apiv1.EventTypeWarning, "SyncNetworkEndpointGroupFailed", "Failed to sync NEG %q %s: %v", s.negName, retryMesg, err)
				}
			} else {
				s.backoff.ResetRetryDelay()
			}

			select {
			case _, open := <-s.syncCh:
				if !open {
					s.stateLock.Lock()
					s.shuttingDown = false
					s.stateLock.Unlock()
					glog.V(2).Infof("Stopping NEG syncer for %s", s.NegSyncerKey.String())
					return
				}
			case <-retryCh:
				// continue to sync
			}
		}
	}()
	return nil
}

func (s *syncer) init() {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	s.stopped = false
	s.syncCh = make(chan interface{}, 1)
}

func (s *syncer) Stop() {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	if !s.stopped {
		glog.V(2).Infof("Stopping NEG syncer for service port %s", s.NegSyncerKey.String())
		s.stopped = true
		s.shuttingDown = true
		close(s.syncCh)
	}
}

func (s *syncer) Sync() bool {
	if s.IsStopped() {
		glog.Warningf("NEG syncer for %s is already stopped.", s.NegSyncerKey.String())
		return false
	}
	select {
	case s.syncCh <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *syncer) IsStopped() bool {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	return s.stopped
}

func (s *syncer) IsShuttingDown() bool {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()
	return s.shuttingDown
}

func (s *syncer) SetSyncFunc(syncFunc func() error) {
	s.syncFunc = syncFunc
}
