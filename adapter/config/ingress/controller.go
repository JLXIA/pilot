// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ingress provides a read-only view of Kubernetes ingress resources
// as an ingress rule configuration type store
package ingress

import (
	"errors"
	"reflect"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"

	"k8s.io/api/extensions/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	proxyconfig "istio.io/api/proxy/v1/config"
	"istio.io/pilot/model"
	"istio.io/pilot/platform/kube"
)

type controller struct {
	mesh         *proxyconfig.ProxyMeshConfig
	domainSuffix string

	client   kubernetes.Interface
	queue    kube.Queue
	informer cache.SharedIndexInformer
	handler  *kube.ChainHandler
}

var (
	errUnsupportedOp = errors.New("unsupported operation: the ingress config store is a read-only view")
)

// NewController creates a new Kubernetes controller
func NewController(client kubernetes.Interface, mesh *proxyconfig.ProxyMeshConfig,
	options kube.ControllerOptions) model.ConfigStoreCache {
	handler := &kube.ChainHandler{}

	// queue requires a time duration for a retry delay after a handler error
	queue := kube.NewQueue(1 * time.Second)

	// informer framework from Kubernetes
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(opts meta_v1.ListOptions) (runtime.Object, error) {
				return client.ExtensionsV1beta1().Ingresses(options.Namespace).List(opts)
			},
			WatchFunc: func(opts meta_v1.ListOptions) (watch.Interface, error) {
				return client.ExtensionsV1beta1().Ingresses(options.Namespace).Watch(opts)
			},
		}, &v1beta1.Ingress{},
		options.ResyncPeriod, cache.Indexers{})

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				queue.Push(kube.NewTask(handler.Apply, obj, model.EventAdd))
			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					queue.Push(kube.NewTask(handler.Apply, cur, model.EventUpdate))
				}
			},
			DeleteFunc: func(obj interface{}) {
				queue.Push(kube.NewTask(handler.Apply, obj, model.EventDelete))
			},
		})

	// first handler in the chain blocks until the cache is fully synchronized
	// it does this by returning an error to the chain handler
	handler.Append(func(obj interface{}, event model.Event) error {
		if !informer.HasSynced() {
			return errors.New("waiting till full synchronization")
		}
		if ingress, ok := obj.(*v1beta1.Ingress); ok {
			glog.V(2).Infof("ingress event %s for %s/%s", event, ingress.Namespace, ingress.Name)
		}
		return nil
	})

	return &controller{
		mesh:         mesh,
		domainSuffix: options.DomainSuffix,
		client:       client,
		queue:        queue,
		informer:     informer,
		handler:      handler,
	}
}

func (c *controller) RegisterEventHandler(typ string, f func(model.Config, model.Event)) {
	c.handler.Append(func(obj interface{}, event model.Event) error {
		ingress := obj.(*v1beta1.Ingress)
		if !shouldProcessIngress(c.mesh, ingress) {
			return nil
		}

		// Convert the ingress into a map[Key]rule, and invoke handler for each
		// TODO: This works well for Add and Delete events, but no so for Update:
		// A updated ingress may also trigger an Add or Delete for one of its constituent sub-rules.
		rules := convertIngress(*ingress, c.domainSuffix)
		for key, rule := range rules {
			config := model.Config{
				Type:    model.IngressRule.Type,
				Key:     key,
				Content: rule,
			}
			f(config, event)
		}

		return nil
	})
}

func (c *controller) HasSynced() bool {
	return c.informer.HasSynced()
}

func (c *controller) Run(stop <-chan struct{}) {
	go c.queue.Run(stop)
	go c.informer.Run(stop)
	<-stop
}

func (c *controller) ConfigDescriptor() model.ConfigDescriptor {
	return model.ConfigDescriptor{model.IngressRule}
}

func (c *controller) Get(typ, key string) (proto.Message, bool, string) {
	if typ != model.IngressRule.Type {
		return nil, false, ""
	}

	ingressName, ingressNamespace, _, _, err := decodeIngressRuleName(key)
	if err != nil {
		glog.V(2).Infof("getIngress(%s) => error %v", key, err)
		return nil, false, ""
	}
	storeKey := kube.KeyFunc(ingressName, ingressNamespace)

	obj, exists, err := c.informer.GetStore().GetByKey(storeKey)
	if err != nil {
		glog.V(2).Infof("getIngress(%s) => error %v", key, err)
		return nil, false, ""
	}
	if !exists {
		return nil, false, ""
	}

	ingress := obj.(*v1beta1.Ingress)
	if !shouldProcessIngress(c.mesh, ingress) {
		return nil, false, ""
	}

	rules := convertIngress(*ingress, c.domainSuffix)
	rule, exists := rules[key]
	return rule, exists, ingress.GetResourceVersion()
}

func (c *controller) List(typ string) ([]model.Config, error) {
	if typ != model.IngressRule.Type {
		return nil, errUnsupportedOp
	}

	out := make([]model.Config, 0)
	for _, obj := range c.informer.GetStore().List() {
		ingress := obj.(*v1beta1.Ingress)
		if shouldProcessIngress(c.mesh, ingress) {
			ingressRules := convertIngress(*ingress, c.domainSuffix)
			for key, rule := range ingressRules {
				out = append(out, model.Config{
					Type:     model.IngressRule.Type,
					Key:      key,
					Revision: ingress.GetResourceVersion(),
					Content:  rule,
				})
			}
		}
	}

	return out, nil
}

func (c *controller) Post(_ proto.Message) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Put(_ proto.Message, _ string) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Delete(_, _ string) error {
	return errUnsupportedOp
}
