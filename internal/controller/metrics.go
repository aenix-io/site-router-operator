/*
Copyright 2026 The Aenix Authors.

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
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Labels of the metrics of a SiteRouter: its namespace and name, which are what an
// administrator tells one from another by.
const (
	labelNamespace = "namespace"
	labelName      = "name"
	labelType      = "type"
	labelStatus    = "status"
)

// Metrics are the series of the operator, keyed by the namespace and name of the
// SiteRouter. A series that is not the state of a SiteRouter any more is removed,
// not left to read as the last thing it was.
type Metrics struct {
	ready     *prometheus.GaugeVec
	condition *prometheus.GaugeVec
	tunnelUp  *prometheus.GaugeVec
	image     *prometheus.GaugeVec
	waiting   *prometheus.GaugeVec

	inProgress prometheus.Gauge
	upgrades   *prometheus.CounterVec
}

// Results of a move to another image, as the metrics say them.
const (
	resultStarted   = "started"
	resultCompleted = "completed"
	resultFailed    = "failed"
)

// NewMetrics registers the metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ready: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "site_router_ready",
			Help: "1 when the SiteRouter is Ready, 0 when it is not.",
		}, []string{labelNamespace, labelName}),
		condition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "site_router_condition",
			Help: "1 for the status a condition of the SiteRouter has now: True, False or Unknown.",
		}, []string{labelNamespace, labelName, labelType, labelStatus}),
		tunnelUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "site_router_tunnel_up",
			Help: "1 when the tunnels of the gateway that are asked for are up, 0 when one is not or none could be read.",
		}, []string{labelNamespace, labelName}),
		image: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "site_router_gateway_image_info",
			Help: "The engine, version and image digest the gateway runs. The value is always 1.",
		}, []string{labelNamespace, labelName, "engine", "version", "digest"}),
	}
	m.waiting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "site_router_upgrade_waiting",
		Help: "1 when the gateway is to move to another image and has not been given a place yet, 0 when it is not.",
	}, []string{labelNamespace, labelName})
	m.inProgress = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "site_router_upgrades_in_progress",
		Help: "How many gateways are moving to another image, which is at most the number the configuration allows.",
	})
	m.upgrades = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "site_router_upgrades_total",
		Help: "Moves of a gateway to another image, by how far they got: started, completed or failed.",
	}, []string{"result"})
	reg.MustRegister(m.ready, m.condition, m.tunnelUp, m.image, m.waiting, m.inProgress, m.upgrades)
	return m
}

// observation is what a pass says of a SiteRouter to the metrics.
type observation struct {
	conditions []metav1.Condition
	engine     v1alpha1.EngineStatus
	// tunnels is what the gateway reported, or nil when it was not asked.
	tunnels map[string]engine.TunnelState
	// wanted is the tunnels that are asked for.
	wanted []string
	// waiting is true when the gateway is to move and has no place yet.
	waiting bool
}

// observe sets the series of a SiteRouter to what a pass found.
func (m *Metrics) observe(key types.NamespacedName, o observation) {
	l := prometheus.Labels{labelNamespace: key.Namespace, labelName: key.Name}

	ready := 0.0
	for _, c := range o.conditions {
		m.condition.DeletePartialMatch(prometheus.Labels{labelNamespace: key.Namespace, labelName: key.Name, labelType: c.Type})
		m.condition.With(prometheus.Labels{
			labelNamespace: key.Namespace, labelName: key.Name, labelType: c.Type, labelStatus: string(c.Status),
		}).Set(1)
		if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue {
			ready = 1
		}
	}
	m.ready.With(l).Set(ready)

	up := 0.0
	if len(o.wanted) > 0 && o.tunnels != nil {
		up = 1
		for _, id := range o.wanted {
			if o.tunnels[id] != engine.TunnelUp {
				up = 0
			}
		}
	}
	m.tunnelUp.With(l).Set(up)

	waiting := 0.0
	if o.waiting {
		waiting = 1
	}
	m.waiting.With(l).Set(waiting)

	m.image.DeletePartialMatch(l)
	if o.engine.Version != "" {
		m.image.With(prometheus.Labels{
			labelNamespace: key.Namespace, labelName: key.Name,
			"engine": o.engine.Name, "version": o.engine.Version, "digest": o.engine.ImageDigest,
		}).Set(1)
	}
}

// forget removes the series of a SiteRouter that is gone.
func (m *Metrics) forget(key types.NamespacedName) {
	l := prometheus.Labels{labelNamespace: key.Namespace, labelName: key.Name}
	m.ready.DeletePartialMatch(l)
	m.condition.DeletePartialMatch(l)
	m.tunnelUp.DeletePartialMatch(l)
	m.image.DeletePartialMatch(l)
	m.waiting.DeletePartialMatch(l)
}

// moving sets how many gateways are moving.
func (m *Metrics) moving(n int) { m.inProgress.Set(float64(n)) }

// upgraded counts a move that started, completed or failed.
func (m *Metrics) upgraded(result string) { m.upgrades.WithLabelValues(result).Inc() }
