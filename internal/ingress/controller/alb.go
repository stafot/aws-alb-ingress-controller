/*
Copyright 2015 The Kubernetes Authors.

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

package controller

import (
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/golang/glog"

	"github.com/eapache/channels"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/albingress"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albacm"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albcache"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albec2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albelbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albiam"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albrgt"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albsession"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albwaf"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations/class"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/metric"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/status"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/sync"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/task"
)

const (
	albHealthPath = "/healthz"
)

// NewALBController creates a new ALB Ingress controller.
func NewALBController(config *Configuration, mc metric.Collector) *ALBController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: config.Client.CoreV1().Events(config.Namespace),
	})

	sess := albsession.NewSession(&aws.Config{MaxRetries: aws.Int(config.AWSAPIMaxRetries)}, config.AWSAPIDebug, mc)
	albcache.NewCache(mc)
	albelbv2.NewELBV2(sess)
	albec2.NewEC2(sess)
	albec2.NewEC2Metadata(sess)
	albacm.NewACM(sess)
	albiam.NewIAM(sess)
	albrgt.NewRGT(sess, config.ClusterName)
	albwaf.NewWAFRegional(sess)

	if config.ALBNamePrefix == "" {
		config.ALBNamePrefix = generateAlbNamePrefix(config.ClusterName)
	}

	glog.Infof("ALB resource names will be prefixed with %s", config.ALBNamePrefix)

	c := &ALBController{
		cfg:             config,
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(config.SyncRateLimit, 1),

		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{
			Component: "aws-alb-ingress-controller",
		}),

		stopCh:   make(chan struct{}),
		updateCh: channels.NewRingChannel(1024),

		stopLock: &sync.RWMutex{},

		runningConfig: new(ingress.Configuration),

		metricCollector: mc,
	}

	c.store = store.New(
		config.Namespace,
		config.ConfigMapName,
		config.ResyncPeriod,
		config.Client,
		c.updateCh)

	c.syncQueue = task.NewTaskQueue(c.syncIngress)
	c.awsSyncQueue = task.NewTaskQueue(c.awsSync)

	c.syncStatus = status.NewStatusSyncer(status.Config{
		Client:              config.Client,
		IngressLister:       c.store,
		ElectionID:          config.ElectionID,
		IngressClass:        class.IngressClass,
		DefaultIngressClass: class.DefaultClass,
		RunningConfig:       c.runningConfig,
	})

	return c
}

// ALBController describes a ALB Ingress controller.
type ALBController struct {
	cfg *Configuration

	mutex sync.RWMutex

	recorder record.EventRecorder

	syncQueue *task.Queue

	awsSyncQueue *task.Queue

	syncStatus status.Sync

	syncRateLimiter flowcontrol.RateLimiter

	// stopLock is used to enforce that only a single call to Stop send at
	// a given time. We allow stopping through an HTTP endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.RWMutex

	stopCh   chan struct{}
	updateCh *channels.RingChannel

	// runningConfig contains the running configuration
	runningConfig *ingress.Configuration

	isShuttingDown bool

	store store.Storer

	metricCollector metric.Collector
}

// Start starts the controller running in the foreground.
func (c *ALBController) Start() {
	glog.Infof("Starting AWS ALB Ingress controller")

	err := c.awsSync(nil)
	if err != nil {
		glog.Fatalf(err.Error())
	}

	c.store.Run(c.stopCh)

	if c.syncStatus != nil {
		go c.syncStatus.Run()
	}

	go c.syncQueue.Run(time.Second, c.stopCh)
	go c.awsSyncQueue.Run(c.cfg.AWSSyncPeriod, c.stopCh)

	// force initial sync
	c.syncQueue.EnqueueTask(task.GetDummyObject("initial-sync"))

	for {
		select {
		case event := <-c.updateCh.Out():
			if c.isShuttingDown {
				break
			}
			if evt, ok := event.(store.Event); ok {
				glog.V(3).Infof("Event %v received - object %v", evt.Type, evt.Obj)
				if evt.Type == store.ConfigurationEvent {
					// TODO: is this necessary? Consider removing this special case
					c.syncQueue.EnqueueTask(task.GetDummyObject("configmap-change"))
					continue
				}

				c.syncQueue.EnqueueSkippableTask(evt.Obj)
			} else {
				glog.Warningf("Unexpected event type received %T", event)
			}
		case <-c.stopCh:
			break
		}
	}
}

// Stop gracefully stops the NGINX master process.
func (c *ALBController) Stop() error {
	c.isShuttingDown = true

	c.stopLock.Lock()
	defer c.stopLock.Unlock()

	if c.syncQueue.IsShuttingDown() {
		return fmt.Errorf("shutdown already in progress")
	}

	glog.Infof("Shutting down controller queues")
	close(c.stopCh)
	go c.syncQueue.Shutdown()
	go c.awsSyncQueue.Shutdown()
	if c.syncStatus != nil {
		c.syncStatus.Shutdown()
	}

	return nil
}

func (c *ALBController) awsSync(i interface{}) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	glog.V(3).Infof("Synchronizing AWS resources")

	// Cache all of the tags for our cluster resources
	r, err := albrgt.RGTsvc.GetClusterResources()
	if err != nil {
		return err
	}

	glog.V(3).Infof("Retrieved tag information on %v load balancers, %v target groups, %v listeners, %v rules, and %v subnets.",
		len(r.LoadBalancers),
		len(r.TargetGroups),
		len(r.Listeners),
		len(r.ListenerRules),
		len(r.Subnets))

	c.runningConfig.Ingresses = albingress.AssembleIngressesFromAWS(&albingress.AssembleIngressesFromAWSOptions{
		Recorder:      c.recorder,
		ALBNamePrefix: c.cfg.ALBNamePrefix,
		Store:         c.store,
		ClusterName:   c.cfg.ClusterName,
	})
	return nil
}

func generateAlbNamePrefix(c string) string {
	hash := crc32.New(crc32.MakeTable(0xedb88320))
	hash.Write([]byte(c))
	return hex.EncodeToString(hash.Sum(nil))
}