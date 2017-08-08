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

package memory

import (
	"reflect"
	"sort"
	"time"

	"github.com/golang/glog"

	"istio.io/pilot/model"
)

type Configs []model.Config

// Handler specifies a function to apply on a Config for a given event type
type Handler func(model.Config, model.Event)

type Monitor interface {
	Start(<-chan struct{})
	AppendEventHandler(string, Handler)
}

type configsMonitor struct {
	store              model.ConfigStore
	configCachedRecord map[string]Configs
	handlers           map[string][]Handler

	ticker   *time.Ticker
	period   time.Duration

	stop <-chan struct{}
}

func NewConfigsMonitor(store model.ConfigStore, period time.Duration) Monitor {
	cache := make(map[string]Configs, 0)
	handlers := make(map[string][]Handler, 0)

	for _, conf := range model.IstioConfigTypes {
		cache[conf.Type] = make(Configs, 0)
		handlers[conf.Type] = make([]Handler, 0)
	}

	return &configsMonitor{
		store:              store,
		period:             period,
		configCachedRecord: cache,
		handlers:           handlers,
	}
}

func (m *configsMonitor) Start(stop <-chan struct{}) {
	m.ticker = time.NewTicker(m.period)
	m.run(stop)
}

func (m *configsMonitor) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			m.ticker.Stop()
		case <-m.ticker.C:
			m.UpdateConfigRecord()
		}
	}
}

func (m *configsMonitor) UpdateConfigRecord() {
	for _, conf := range model.IstioConfigTypes {
		configs, err := m.store.List(conf.Type)
		if err != nil {
			glog.Warningf("Unable to fetch configs of type: %s", conf.Type)
			return
		}
		newRecord := Configs(configs)
		newRecord.normalize()
		m.compareToCache(conf.Type, m.configCachedRecord[conf.Type], newRecord)
		m.configCachedRecord[conf.Type] = newRecord
	}
}

func (m *configsMonitor) compareToCache(typ string, oldRec, newRec Configs) {
	io, in := 0, 0
	for io < len(oldRec) && in < len(newRec) {
		if reflect.DeepEqual(oldRec[io], newRec[in]) {
		} else if oldRec[io].Key == newRec[in].Key {
			// An update event
			m.applyHandlers(typ, newRec[in], model.EventUpdate)
		} else if oldRec[io].Key < newRec[in].Key {
			// A delete event
			m.applyHandlers(typ, oldRec[io], model.EventDelete)
			in--
		} else {
			// An add event
			m.applyHandlers(typ, newRec[in], model.EventAdd)
			io--
		}
		io++
		in++
	}

	for ; io < len(oldRec); io++ {
		m.applyHandlers(typ, oldRec[io], model.EventDelete)
	}

	for ; in < len(newRec); in++ {
		m.applyHandlers(typ, newRec[in], model.EventAdd)
	}
}

func (m *configsMonitor) AppendEventHandler(typ string, h Handler) {
	m.handlers[typ] = append(m.handlers[typ], h)
}

func (m *configsMonitor) applyHandlers(typ string, config model.Config, e model.Event) {
	for _, f := range m.handlers[typ] {
		f(config, e)
	}
}

func (list Configs) normalize() {
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
}
