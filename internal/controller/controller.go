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

// Package controller is the reconcile loop of the operator: it makes the gateway
// of a SiteRouter exist, configures it through the engine, asks the connector for
// the routes to it, and says in the status how that stands.
//
// A pass reads the cluster and the gateway and keeps nothing but the revision
// the engine last applied. What the loop knows is in the objects, so that an
// operator that restarts picks up where the last one was.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/desired"
	"github.com/aenix-io/site-router-operator/internal/forwarder"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
	"github.com/aenix-io/site-router-operator/internal/rollout"
)

// Finalizer holds a SiteRouter until the routes to its gateway are taken away,
// so that nothing is routed to a gateway that is about to go.
const Finalizer = "siterouter.aenix.io/finalizer"

const (
	// retryAfter is how soon a gateway that is not ready is looked at again. What
	// the operator watches tells it of most changes; this is for the ones it does
	// not.
	retryAfter = 10 * time.Second
	// withdrawRetry is how soon a SiteRouter that is being deleted is looked at
	// again while its routes are not gone yet.
	withdrawRetry = 5 * time.Second
	// pskBytes is the length of a generated pre-shared key, before it is written as
	// text.
	pskBytes = 32
)

// Reasons the controller gives that the API does not name, because they are said
// while everything is fine or about nothing a tenant did.
const (
	reasonRunning     = "Running"
	reasonNotReady    = "GatewayNotReady"
	messageNotReady   = "the gateway is not ready, so nothing was said about this"
	messagePeerAbsent = "the address of the peer is not set: the gateway is configured and has no tunnel to bring up"
)

// Options are what a Reconciler is made of. Every field but Now, Rand and Metrics
// is required.
type Options struct {
	// Client reads and writes the objects of the operator: the SiteRouters and the
	// Forwarders. It is the one of the manager, so that they are watched.
	Client client.Client
	// Reader reads everything else, from the apiserver and not from a cache: the
	// Secrets, the Service of the tunnel and the VM of the gateway are not worth an
	// informer on every namespace. It is also what decides whether an object is the
	// operator's: the cache of the Client holds only what carries the label of the
	// operator, so it does not show an object that someone else made under the name
	// of one of the gateway, and one that is not shown is applied over.
	Reader   client.Reader
	Recorder events.EventRecorder
	Config   config.Config
	Engine   engine.Engine
	Networks *denyset.Source
	// Metrics is where the series go. With none, none are kept.
	Metrics *Metrics
	// Now is the clock, and is time.Now when nil.
	Now func() time.Time
	// Rand is the source of the pre-shared keys, and is crypto/rand when nil.
	Rand io.Reader
}

// Reconciler reconciles SiteRouters.
type Reconciler struct {
	client     client.Client
	reader     client.Reader
	recorder   events.EventRecorder
	config     config.Config
	engine     engine.Engine
	networks   *denyset.Source
	metrics    *Metrics
	now        func() time.Time
	rand       io.Reader
	stepper    *reconcile.Stepper
	applier    *gateway.Applier
	forwarders *forwarder.Manager
	gate       *rollout.Gate
	revisions  revisions

	// restored is true once the places of the moves that were under way have been
	// taken, which is before the first pass.
	restoreMu sync.Mutex
	restored  bool
	// unsupported is the version an annotation named that is not listed, for each
	// SiteRouter that has said so, so that it is said once.
	unsupportedMu sync.Mutex
	unsupported   map[types.NamespacedName]string
}

// New returns a Reconciler.
func New(o Options) (*Reconciler, error) {
	switch {
	case o.Client == nil || o.Reader == nil:
		return nil, errors.New("controller: a Client and a Reader are required")
	case o.Recorder == nil:
		return nil, errors.New("controller: a Recorder is required")
	case o.Engine == nil:
		return nil, errors.New("controller: an Engine is required")
	case o.Networks == nil:
		return nil, errors.New("controller: Networks are required")
	}
	if err := o.Config.Validate(); err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	if err := o.Config.CheckAgainst(o.Engine.Info()); err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	stepper, err := reconcile.New(o.Engine, o.Networks.Validator())
	if err != nil {
		return nil, err
	}
	r := &Reconciler{
		client:   o.Client,
		reader:   o.Reader,
		recorder: o.Recorder,
		config:   o.Config,
		engine:   o.Engine,
		networks: o.Networks,
		metrics:  o.Metrics,
		now:      o.Now,
		rand:     o.Rand,
		stepper:  stepper,
		applier:  &gateway.Applier{Client: o.Client},
		forwarders: &forwarder.Manager{
			Client:             o.Client,
			Reader:             o.Reader,
			NotRespondingAfter: o.Config.Timing.ForwarderNotResponding.Duration,
			Now:                o.Now,
		},
		gate:        rollout.NewGate(o.Config.Upgrade.MaxConcurrent),
		revisions:   revisions{m: map[types.NamespacedName]revision{}},
		unsupported: map[types.NamespacedName]string{},
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.rand == nil {
		r.rand = rand.Reader
	}
	return r, nil
}

// outcome is what a pass found, to be written to the status.
type outcome struct {
	status  v1alpha1.SiteRouterStatus
	tunnels map[string]engine.TunnelState
	wanted  []string
	requeue time.Duration
	// notes are events the pass has to say that no condition says.
	notes []note
	// completed is true when a gateway has finished moving to another image.
	completed bool
	// waiting is true when a gateway is to move and has no place yet.
	waiting bool
	// err is why the pass is to be tried again at once, with a back-off. It is set
	// for what is not for a human to put right; the rest waits for the next pass.
	err error
}

// Reconcile makes the gateway of a SiteRouter what the SiteRouter says.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sr := &v1alpha1.SiteRouter{}
	if err := r.client.Get(ctx, req.NamespacedName, sr); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if err := r.restoreGate(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if !sr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, sr)
	}
	if !controllerutil.ContainsFinalizer(sr, Finalizer) {
		orig := sr.DeepCopy()
		controllerutil.AddFinalizer(sr, Finalizer)
		if err := r.client.Patch(ctx, sr, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	// Writing the status brings the object up to date with what was written, so
	// what it said before is kept to tell what changed.
	before := sr.Status.DeepCopy().Conditions
	out := r.pass(ctx, sr)
	if err := r.writeStatus(ctx, sr, out.status); err != nil {
		return ctrl.Result{}, err
	}
	r.publish(sr, before, out)
	if out.err != nil {
		return ctrl.Result{}, out.err
	}
	return ctrl.Result{RequeueAfter: out.requeue}, nil
}

// writeStatus writes the status when it differs from the one the object has.
func (r *Reconciler) writeStatus(ctx context.Context, sr *v1alpha1.SiteRouter, status v1alpha1.SiteRouterStatus) error {
	if equality.Semantic.DeepEqual(sr.Status, status) {
		return nil
	}
	orig := sr.DeepCopy()
	sr.Status = status
	if err := r.client.Status().Patch(ctx, sr, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("writing the status: %w", err)
	}
	return nil
}

// pass runs one pass for a SiteRouter that is not being deleted.
func (r *Reconciler) pass(ctx context.Context, sr *v1alpha1.SiteRouter) outcome {
	log := logf.FromContext(ctx)
	st := *sr.Status.DeepCopy()
	st.ObservedGeneration = sr.Generation
	conds := newConditions(sr.Status.Conditions, sr.Generation)
	out := outcome{requeue: r.config.Timing.Poll.Duration}

	gw := r.makeGateway(ctx, sr, &st)
	routes := r.forwarders.For(sr)

	var scope []string
	if gw.objects {
		if err := routes.Ensure(ctx); err != nil {
			gw.fail(err)
		} else if rep, err := routes.Report(ctx); err != nil {
			gw.fail(err)
		} else {
			scope = rep.Scope
		}
	}

	// What is declared is judged on every pass, whether or not the gateway is up,
	// so that a tenant whose networks are refused is told at once and not when the
	// VM has booted.
	var (
		res   reconcile.Result
		built desired.Output
	)
	nets, err := r.networks.Networks(ctx)
	if err == nil {
		built, err = desired.Build(desired.Input{
			Instance:       sr,
			TunnelAddress:  gw.tunnelAddress,
			PSK:            gw.psk,
			ManagementCIDR: r.config.ManagementCIDR,
			Scope:          scope,
			Networks:       nets,
			NICMACs:        gw.macs,
		})
	}
	switch {
	case err != nil:
		res = reconcile.Unavailable(err)
	default:
		in := reconcile.Instance{Name: sr.Name, RemoteCIDRs: built.RemoteCIDRs, Desired: built.Desired, Routes: routes}
		if gw.ready() {
			res = r.stepper.Step(ctx, gw.endpoint, in, r.revisions.of(sr))
		} else {
			res = r.stepper.Judge(ctx, in)
		}
		if built.DroppedDestinations > 0 {
			log.Info("the gateway is not opened to addresses outside the networks of the cluster", "dropped", built.DroppedDestinations)
		}
		for _, t := range built.Desired.Tunnels {
			out.wanted = append(out.wanted, t.ID)
		}
	}

	conds.set(v1alpha1.ConditionGatewayReady, gw.ready(), gw.shownReason(), gw.shownMessage())
	conds.fromStep(res)
	if c, ok := res.Conditions[reconcile.CondValidated]; ok && c.OK && gw.ready() && !built.TunnelConfigured && sr.Spec.Peer.Address == "" {
		conds.put(v1alpha1.ConditionValidated, metav1.ConditionTrue, v1alpha1.ReasonPeerNotConfigured, messagePeerAbsent)
	}
	st.Conditions = conds.finish(reasonNotReady, messageNotReady)
	out.status = st
	out.tunnels = res.Tunnels
	out.notes, out.completed, out.waiting = gw.notes, gw.completed, gw.waiting

	if res.Err != nil {
		log.Info("the pass could not go on", "reason", res.Err.Error())
	}
	out.err = errors.Join(gw.retry, res.Err)
	if !gw.ready() {
		out.requeue = min(out.requeue, retryAfter)
	}
	return out
}

// gatewayView is what a pass learns of the gateway while it makes it exist.
type gatewayView struct {
	// objects is true when the objects of the gateway are in the cluster as they
	// should be.
	objects bool
	// reason and message say why the gateway cannot be configured yet: it does not
	// run, it has no address, or what its tunnel needs is not there. Both are empty
	// when it can.
	reason  string
	message string
	// retry is the error that is to be tried again with a back-off: a failure that
	// is no one's to put right.
	retry error
	// secrets are what is known of the pass that must not reach a condition, an
	// event or the log, as it is learned.
	secrets []string
	// okReason and okMessage are said for a gateway that can be configured, and say
	// that something is on besides: a disk that is being made for another image, or
	// a move that was given up. They are empty when there is nothing to say.
	okReason  string
	okMessage string
	// notes are events the pass has to say.
	notes []note
	// completed is true when the gateway has finished moving to another image.
	completed bool
	// waiting is true when the gateway is to move and has no place yet.
	waiting bool

	endpoint      engine.Endpoint
	tunnelAddress string
	psk           string
	macs          []string
}

func (g *gatewayView) ready() bool { return g.reason == "" }

// shownReason and shownMessage are what the GatewayReady condition says.
func (g *gatewayView) shownReason() string {
	switch {
	case g.reason != "":
		return g.reason
	case g.okReason != "":
		return g.okReason
	}
	return reasonRunning
}

func (g *gatewayView) shownMessage() string {
	if g.reason == "" {
		return g.okMessage
	}
	return g.message
}

// info records what is on besides a gateway that can be configured.
func (g *gatewayView) info(reason, message string) { g.okReason, g.okMessage = reason, message }

func (g *gatewayView) pending(reason, message string) {
	if g.reason == "" {
		g.reason, g.message = reason, message
	}
}

// fail records why the gateway cannot be made what it should be. The failures
// that are for someone to put right, such as an object that is not the
// operator's, are named and left for the next pass; the rest are tried again with
// a back-off. What comes back from the engine, the apiserver or a connector is not
// trusted to keep out of its errors what it was given, so the text is redacted
// before it is shown to a tenant or written to a log.
func (g *gatewayView) fail(err error) {
	g.objects = false
	g.reason, g.message = "", ""
	msg := reconcile.Redact(err.Error(), g.secrets)
	switch {
	case errors.Is(err, gateway.ErrIsolationUnavailable):
		g.pending(v1alpha1.ReasonGatewayIsolationUnavailable, msg)
	case isConflict(err):
		g.pending(v1alpha1.ReasonObjectConflict, msg)
	case isLoadBalancerClass(err):
		g.pending(v1alpha1.ReasonLoadBalancerClassChanged, msg)
	default:
		g.pending(v1alpha1.ReasonReconcileFailed, msg)
		g.retry = errors.New(msg)
	}
}

func isConflict(err error) bool {
	_, notOwned := errors.AsType[*gateway.NotOwnedError](err)
	_, mismatch := errors.AsType[*gateway.ImageMismatchError](err)
	return notOwned || mismatch
}

func isLoadBalancerClass(err error) bool {
	_, ok := errors.AsType[*gateway.LoadBalancerClassError](err)
	return ok
}

// inputs are what the objects of a gateway are made from besides the image.
type inputs struct {
	psk, pskPending string
	// existing are the credentials the engine minted for the gateway, which it keeps.
	existing map[string][]byte
}

// collect reads the key of the tunnel and the credentials of the gateway, and notes
// them as secrets.
func (r *Reconciler) collect(ctx context.Context, sr *v1alpha1.SiteRouter, st *v1alpha1.SiteRouterStatus, g *gatewayView) (inputs, error) {
	var in inputs
	psk, pskName, pskPending, err := r.preSharedKey(ctx, sr)
	if err != nil {
		return in, err
	}
	st.PSKSecretRef = &v1alpha1.LocalSecretRef{Name: pskName}
	in.psk, in.pskPending = psk, pskPending
	if psk != "" {
		g.secrets = append(g.secrets, psk)
	}

	names := gateway.NamesFor(sr.Name)
	keySecret, err := r.getSecret(ctx, sr.Namespace, names.APIKey)
	if err != nil {
		return in, err
	}
	if keySecret != nil {
		if !metav1.IsControlledBy(keySecret, sr) {
			return in, &gateway.NotOwnedError{Kind: "Secret", Name: names.APIKey}
		}
		in.existing = keySecret.Data
		g.secrets = append(g.secrets, engine.Endpoint{Credentials: in.existing}.Secrets()...)
	}
	return in, nil
}

// render asks the engine for the seed of a version and renders the objects of the
// gateway for its image. An image no version is listed for runs with the seed of the
// latest: its own exists already and is never written again.
func (r *Reconciler) render(sr *v1alpha1.SiteRouter, d rollout.Decision, in inputs, g *gatewayView) (engine.BootstrapPlan, []client.Object, error) {
	version := d.Target
	if d.Orphaned {
		version = r.config.Engine.Latest
	}
	plan, err := r.engine.Bootstrap(engine.BootstrapRequest{
		Instance:        sr.Name,
		Namespace:       sr.Namespace,
		ManagementAllow: r.config.ManagementCIDR,
		Version:         version,
		Existing:        in.existing,
	})
	if err != nil {
		return plan, nil, fmt.Errorf("asking the engine for the seed of version %s: %w", version, err)
	}
	// What the engine minted is as secret as what it was given.
	g.secrets = append(g.secrets, engine.Endpoint{Credentials: plan.Credentials}.Secrets()...)

	var ownedPSK []byte
	if gateway.OwnsPSKSecret(sr) {
		ownedPSK = []byte(in.psk)
	}
	objs, err := gateway.Render(gateway.Input{
		Instance: sr, Config: r.config.Gateway, Image: d.Image, Plan: plan, PSK: ownedPSK,
	})
	return plan, objs, err
}

// imageObjects are the disk and the seed among the objects, which are what depends
// on the image.
func imageObjects(objs []client.Object, n gateway.Names) []client.Object {
	var out []client.Object
	for _, o := range objs {
		if o.GetName() == n.BootDisk || o.GetName() == n.CloudInit {
			out = append(out, o)
		}
	}
	return out
}

// makeGateway makes the objects of the gateway exist, moves it to another image
// when it is to move, and reads how it stands. It never returns an error: what
// went wrong is in the view, so that the pass can still tell the tenant what is
// wrong with what they declared.
func (r *Reconciler) makeGateway(ctx context.Context, sr *v1alpha1.SiteRouter, st *v1alpha1.SiteRouterStatus) *gatewayView {
	g := &gatewayView{}
	names := gateway.NamesFor(sr.Name)
	key := client.ObjectKeyFromObject(sr)

	vm, vmi, err := r.readVM(ctx, sr, names.VM)
	if err != nil {
		g.fail(err)
		return g
	}
	cur, err := r.current(ctx, sr, vm, vmi)
	if err != nil {
		g.fail(err)
		return g
	}
	dec, err := rollout.Decide(r.config, cur, sr.Annotations[v1alpha1.AnnotationUpgradeTo])
	if err != nil {
		g.fail(err)
		return g
	}
	r.warnUnsupported(key, dec, g)

	in, err := r.collect(ctx, sr, st, g)
	if err != nil {
		g.fail(err)
		return g
	}

	// What the gateway runs is what it is made of in this pass. A gateway that is to
	// move is made of the image it runs until the disk of the other is ready, and
	// that disk is made beside it.
	run, phase, message := dec, movePhaseNone, ""
	if dec.Moving {
		if run, err = rollout.Stay(r.config, cur); err == nil {
			phase, message, err = r.planMove(ctx, sr, dec)
		}
		if err != nil {
			g.fail(err)
			return g
		}
	}
	// A pass that fails after it took a place for a move that is not marked in the
	// status gives it back, so that a gateway that cannot move does not hold one.
	failed := func(err error) *gatewayView {
		g.fail(err)
		if !moving(sr.Status.Conditions) {
			r.gate.Release(key)
		}
		return g
	}
	runNames := names.ForImage(run.Image)
	arrived := vmi != nil && vmi.running() && vmi.BootDisk == runNames.BootDisk
	// A gateway that was moving and does not run on the disk it is pointed at is
	// restarting. A move holds its place until the gateway runs on the new disk, and
	// the status does not say it runs an image it has not booted on, whichever way it
	// is going.
	restarting := moving(sr.Status.Conditions) && !arrived
	holding := !dec.Moving && restarting
	if !dec.Moving && !holding {
		r.gate.Release(key)
	}

	plan, objs, err := r.render(sr, run, in, g)
	if err != nil {
		return failed(err)
	}
	if err := r.applier.Apply(ctx, sr, objs); err != nil {
		return failed(err)
	}
	g.objects = true

	if err := r.advance(ctx, sr, g, vmi, dec, phase, message, in); err != nil {
		return failed(err)
	}
	if phase == movePhaseSwap {
		vmi = nil
	}

	if !restarting {
		digest, err := gateway.ImageDigest(run.Image)
		if err != nil {
			return failed(err)
		}
		// It is a move that is completed when the gateway runs another image than the
		// status knew; one that is given up on, by taking the annotation away, is not.
		moved := moving(sr.Status.Conditions) && digest != sr.Status.Engine.ImageDigest
		st.Engine = v1alpha1.EngineStatus{Name: r.config.Engine.Name, Version: run.Target, ImageDigest: digest}
		if moved {
			g.completed = true
			g.notes = append(g.notes, note{
				eventType: corev1.EventTypeNormal, reason: eventUpgradeCompleted,
				message: fmt.Sprintf("the gateway runs version %s", run.Target),
			})
		}
		if !dec.Moving && arrived {
			if err := r.cleanup(ctx, sr, runNames); err != nil {
				logf.FromContext(ctx).Error(err, "removing what the gateway no longer runs from")
				g.retry = errors.Join(g.retry, err)
			}
		}
	}
	if holding {
		g.pending(v1alpha1.ReasonUpgrading, fmt.Sprintf("restarting on version %s", run.Target))
	}

	return r.observe(ctx, sr, st, g, in, plan, vmi)
}

// advance does the step of a move that the phase calls for.
func (r *Reconciler) advance(ctx context.Context, sr *v1alpha1.SiteRouter, g *gatewayView, vmi *vmiFacts, dec rollout.Decision, phase movePhase, message string, in inputs) error {
	switch phase {
	case movePhaseNone:
		return nil
	case movePhaseWaiting:
		g.waiting = true
		return nil
	case movePhaseFailed:
		g.info(v1alpha1.ReasonUpgradeFailed, message)
		return nil
	}

	newNames := gateway.NamesFor(sr.Name).ForImage(dec.Image)
	_, objs, err := r.render(sr, dec, in, g)
	if err != nil {
		return err
	}
	if phase == movePhaseImporting {
		if err := r.applier.Apply(ctx, sr, imageObjects(objs, newNames)); err != nil {
			return err
		}
		g.info(v1alpha1.ReasonUpgrading, message)
		return nil
	}
	if err := r.applier.Apply(ctx, sr, objs); err != nil {
		return err
	}
	if err := r.boot(ctx, sr, vmi, newNames.BootDisk); err != nil {
		return err
	}
	g.pending(v1alpha1.ReasonUpgrading, message)
	return nil
}

// observe reads how a gateway that has its objects stands: the address of the tunnel
// and the instance of the VM.
func (r *Reconciler) observe(ctx context.Context, sr *v1alpha1.SiteRouter, st *v1alpha1.SiteRouterStatus, g *gatewayView, in inputs, plan engine.BootstrapPlan, vmi *vmiFacts) *gatewayView {
	svc := &corev1.Service{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: sr.Namespace, Name: gateway.NamesFor(sr.Name).Tunnel}, svc); err != nil && !apierrors.IsNotFound(err) {
		g.fail(err)
		return g
	}
	g.tunnelAddress = tunnelAddress(svc)
	st.TunnelAddress = g.tunnelAddress
	g.psk = in.psk

	switch {
	case vmi == nil:
		g.pending(v1alpha1.ReasonGatewayPending, "the gateway VM has not started yet")
	case !vmi.running():
		g.pending(v1alpha1.ReasonGatewayPending, fmt.Sprintf("the gateway VM is not running yet (phase %q)", vmi.Phase))
	default:
		g.endpoint = engine.Endpoint{
			Address:     net.JoinHostPort(vmi.IP, strconv.Itoa(plan.ManagementPort)),
			Credentials: plan.Credentials,
		}
		g.macs = vmi.MACs
	}
	if g.tunnelAddress == "" {
		g.pending(v1alpha1.ReasonTunnelAddressPending, "the load balancer has not given the tunnel an address yet")
	}
	if in.pskPending != "" {
		g.pending(v1alpha1.ReasonPSKSecretPending, in.pskPending)
	}
	return g
}

// preSharedKey returns the pre-shared key of the tunnel and the name of the Secret
// that holds it. The key is empty, with a reason, while a Secret that the
// SiteRouter names is not there or holds none. A Secret the operator makes is
// made once: the key it holds is generated by the first pass that finds none.
func (r *Reconciler) preSharedKey(ctx context.Context, sr *v1alpha1.SiteRouter) (key, name, pending string, err error) {
	if !gateway.OwnsPSKSecret(sr) {
		name = sr.Spec.Peer.Auth.SecretRef.Name
		s, err := r.getSecret(ctx, sr.Namespace, name)
		switch {
		case err != nil:
			return "", name, "", err
		case s == nil:
			return "", name, fmt.Sprintf("the Secret %q that holds the pre-shared key does not exist", name), nil
		case controlledBySiteRouter(s):
			// The operator reads this Secret with its own rights, on the word of
			// whoever wrote the SiteRouter, who may not be allowed to read it. The key
			// of another gateway is the one it must not hand over.
			return "", name, fmt.Sprintf("the Secret %q belongs to a SiteRouter, and the key of one gateway is not used for another", name), nil
		case len(s.Data["psk"]) == 0:
			return "", name, fmt.Sprintf("the Secret %q has no key \"psk\"", name), nil
		}
		return string(s.Data["psk"]), name, "", nil
	}

	name = gateway.NamesFor(sr.Name).PSK
	s, err := r.getSecret(ctx, sr.Namespace, name)
	if err != nil {
		return "", name, "", err
	}
	if s == nil {
		key, err = r.generateKey()
		return key, name, "", err
	}
	if !metav1.IsControlledBy(s, sr) {
		return "", name, "", &gateway.NotOwnedError{Kind: "Secret", Name: name}
	}
	if len(s.Data["psk"]) == 0 {
		return "", name, "", fmt.Errorf("the Secret %s has no key \"psk\", and the operator does not write a Secret it has made", name)
	}
	return string(s.Data["psk"]), name, "", nil
}

// controlledBySiteRouter says whether a SiteRouter controls the object, which is
// then the operator's own and made for a gateway.
func controlledBySiteRouter(o client.Object) bool {
	owner := metav1.GetControllerOf(o)
	if owner == nil {
		return false
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	return err == nil && gv.Group == v1alpha1.GroupVersion.Group && owner.Kind == "SiteRouter"
}

func (r *Reconciler) generateKey() (string, error) {
	b := make([]byte, pskBytes)
	if _, err := io.ReadFull(r.rand, b); err != nil {
		return "", fmt.Errorf("generating a pre-shared key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// revision is what the engine last applied to a gateway, with the SiteRouter it
// was applied for: a SiteRouter made again under the same name starts over.
type revision struct {
	uid   types.UID
	state *reconcile.State
}

type revisions struct {
	mu sync.Mutex
	m  map[types.NamespacedName]revision
}

// of returns the state of a SiteRouter, which a pass changes in place.
func (r *revisions) of(sr *v1alpha1.SiteRouter) *reconcile.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := client.ObjectKeyFromObject(sr)
	if e, ok := r.m[key]; ok && e.uid == sr.UID {
		return e.state
	}
	e := revision{uid: sr.UID, state: &reconcile.State{}}
	r.m[key] = e
	return e.state
}

func (r *revisions) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}

// forget drops what the controller keeps of a SiteRouter that is gone.
func (r *Reconciler) forget(key types.NamespacedName) {
	r.revisions.forget(key)
	r.gate.Release(key)
	r.unsupportedMu.Lock()
	delete(r.unsupported, key)
	r.unsupportedMu.Unlock()
	if r.metrics != nil {
		r.metrics.forget(key)
	}
}
