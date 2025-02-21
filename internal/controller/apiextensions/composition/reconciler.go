/*
Copyright 2021 The Crossplane Authors.

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

// Package composition creates composition revisions.
package composition

import (
	"context"
	"strconv"
	"strings"
	"time"

	mcctrl "github.com/multicluster-runtime/multicluster-runtime"
	mcclient "github.com/multicluster-runtime/multicluster-runtime/pkg/client"
	mcmanager "github.com/multicluster-runtime/multicluster-runtime/pkg/manager"
	"github.com/multicluster-runtime/multicluster-runtime/pkg/multicluster"
	mcreconciler "github.com/multicluster-runtime/multicluster-runtime/pkg/reconcile"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/internal/controller/apiextensions/controller"
)

const (
	timeout = 2 * time.Minute
)

// Error strings.
const (
	errGet             = "cannot get Composition"
	errListRevs        = "cannot list CompositionRevisions"
	errCreateRev       = "cannot create CompositionRevision"
	errOwnRev          = "cannot own CompositionRevision"
	errUpdateRevStatus = "cannot update CompositionRevision status"
	errUpdateRevSpec   = "cannot update CompositionRevision spec"
)

// Event reasons.
const (
	reasonCreateRev event.Reason = "CreateRevision"
	reasonUpdateRev event.Reason = "UpdateRevision"
)

// Setup adds a controller that reconciles Compositions by creating new
// CompositionRevisions for each revision of the Composition's spec.
func Setup(mgr mcctrl.Manager, o controller.TypedOptions[mcreconciler.Request]) error {
	name := "revisions/" + strings.ToLower(v1.CompositionGroupKind)

	rec := multicluster.Func[event.Recorder](func(clusterName string) (event.Recorder, error) {
		cl, err := mgr.GetCluster(clusterName)
		if err != nil {
			return nil, err
		}
		return event.NewAPIRecorder(cl.GetEventRecorderFor(name)), nil
	})

	r := NewReconciler(mgr,
		WithLogger(o.Logger.WithValues("controller", name)),
		WithRecorder(rec))

	return mcctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1.Composition{}).
		Owns(&v1.CompositionRevision{}).
		WithOptions(o.ForControllerRuntime()).
		Complete(ratelimiter.NewTypedReconciler[mcreconciler.Request](name, errors.WithTypedSilentRequeueOnConflict(r), o.GlobalRateLimiter))
}

// ReconcilerOption is used to configure the Reconciler.
type ReconcilerOption func(*Reconciler)

// WithLogger specifies how the Reconciler should log messages.
func WithLogger(log logging.Logger) ReconcilerOption {
	return func(r *Reconciler) {
		r.log = log
	}
}

// WithRecorder specifies how the Reconciler should record Kubernetes events.
func WithRecorder(er multicluster.Func[event.Recorder]) ReconcilerOption {
	return func(r *Reconciler) {
		r.record = er
	}
}

// NewReconciler returns a Reconciler of Compositions.
func NewReconciler(mgr mcmanager.Manager, opts ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		client: mgr.GetClient(),
		log:    logging.NewNopLogger(),
		record: multicluster.Static[event.Recorder](event.NewNopRecorder()),
	}

	for _, f := range opts {
		f(r)
	}
	return r
}

// A Reconciler reconciles Compositions by creating new CompositionRevisions for
// each revision of the Composition's spec.
type Reconciler struct {
	client mcclient.Client

	log    logging.Logger
	record multicluster.Func[event.Recorder]
}

// Reconcile a Composition.
func (r *Reconciler) Reconcile(ctx context.Context, req mcreconciler.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", req)
	log.Debug("Reconciling")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	comp := &v1.Composition{}
	cli, err := r.client(req.ClusterName)
	if err != nil {
		return reconcile.Result{}, err
	}
	rec, err := r.record(req.ClusterName)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := cli.Get(ctx, req.NamespacedName, comp); err != nil {
		log.Debug(errGet, "error", err)
		rec.Event(comp, event.Warning(reasonCreateRev, errors.Wrap(err, errGet)))
		return reconcile.Result{}, errors.Wrap(resource.IgnoreNotFound(err), errGet)
	}

	if meta.WasDeleted(comp) {
		return reconcile.Result{}, nil
	}

	currentHash := comp.Hash()

	log = log.WithValues(
		"uid", comp.GetUID(),
		"version", comp.GetResourceVersion(),
		"name", comp.GetName(),
		"spec-hash", currentHash,
	)

	rl := &v1.CompositionRevisionList{}
	if err := cli.List(ctx, rl, client.MatchingLabels{v1.LabelCompositionName: comp.GetName()}); err != nil {
		log.Debug(errListRevs, "error", err)
		rec.Event(comp, event.Warning(reasonCreateRev, errors.Wrap(err, errListRevs)))
		return reconcile.Result{}, errors.Wrap(err, errListRevs)
	}

	var latestRev, existingRev int64

	if lr := v1.LatestRevision(comp, rl.Items); lr != nil {
		latestRev = lr.Spec.Revision
	}

	for i := range rl.Items {
		rev := &rl.Items[i]

		if !metav1.IsControlledBy(rev, comp) {
			// We already listed revisions with Composition name label pointing
			// to this Composition. Let's make sure they are controlled by it.
			// Note(turkenh): Owner references are stripped out when a resource
			// is moved from one cluster to another (i.e. backup/restore) since
			// the UID of the owner is not preserved. We need to make sure to
			// re-add the owner reference to all revisions of this Composition.
			if err := meta.AddControllerReference(rev, meta.AsController(meta.TypedReferenceTo(comp, v1.CompositionGroupVersionKind))); err != nil {
				log.Debug(errOwnRev, "error", err)
				rec.Event(comp, event.Warning(reasonUpdateRev, err))
				return reconcile.Result{}, errors.Wrap(err, errOwnRev)
			}
			if err := cli.Update(ctx, rev); err != nil {
				log.Debug(errOwnRev, "error", err)
				rec.Event(comp, event.Warning(reasonUpdateRev, err))
				return reconcile.Result{}, errors.Wrap(err, errOwnRev)
			}
		}

		// This revision does not match our current Composition.
		if rev.GetLabels()[v1.LabelCompositionHash] != currentHash[:63] {
			continue
		}

		// This revision matches our current Composition. We don't need a new one.
		existingRev = rev.Spec.Revision

		// This revision has the highest revision number - it doesn't need updating.
		if rev.Spec.Revision == latestRev {
			continue
		}

		// This revision does not have the highest revision number. Update it so that it does.
		rev.Spec.Revision = latestRev + 1
		if err := cli.Update(ctx, rev); err != nil {
			log.Debug(errUpdateRevSpec, "error", err)
			if kerrors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			rec.Event(comp, event.Warning(reasonUpdateRev, err))
			return reconcile.Result{}, errors.Wrap(err, errUpdateRevSpec)
		}
	}

	// We start from revision 1, so 0 indicates we didn't find one.
	if existingRev > 0 {
		log.Debug("No new revision needed.", "current-revision", existingRev)
		return reconcile.Result{}, nil
	}

	if err := cli.Create(ctx, NewCompositionRevision(comp, latestRev+1)); err != nil {
		log.Debug(errCreateRev, "error", err)
		rec.Event(comp, event.Warning(reasonCreateRev, err))
		return reconcile.Result{}, errors.Wrap(err, errCreateRev)
	}

	log.Debug("Created new revision", "revision", latestRev+1)
	rec.Event(comp, event.Normal(reasonCreateRev, "Created new revision", "revision", strconv.FormatInt(latestRev+1, 10)))
	return reconcile.Result{}, nil
}
