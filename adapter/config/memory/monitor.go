package memory

import (
	"reflect"
	"sort"
	"time"

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

	ticker   time.Ticker
	period   time.Duration
	tickChan <-chan time.Time

	stop      <-chan struct{}
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
	m.tickChan = time.NewTicker(m.period).C
	m.run(stop)
}

func (m *configsMonitor) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			m.tickChan.Stop()
		case <-m.tickChan:
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
		if !reflect.DeepEqual(newRecord, m.configCachedRecord[conf.Type]) {
			// TODO: Make proper comparison to the cache record
			obj := model.Config{}
			var event model.Event
			for _, f := range m.handlers[conf.Type] {
				f(obj, event)
			}
			m.configCachedRecord[conf.Type] = newRecord
		}
	}

	return nil
}

func (m *configsMonitor) AppendEventHandler(typ string, h Handler) {
	m.handlers[typ] = append(m.handlers[typ], h)
}

func (list Configs) normalize() {
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
}
