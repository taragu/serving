/*
Copyright 2018 The Knative Authors

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

package service

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving/v1beta1"
	listers "github.com/knative/serving/pkg/client/listers/serving/v1alpha1"
	"github.com/knative/serving/pkg/reconciler"
	cfgreconciler "github.com/knative/serving/pkg/reconciler/configuration"
	"github.com/knative/serving/pkg/reconciler/service/resources"
	resourcenames "github.com/knative/serving/pkg/reconciler/service/resources/names"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
)

const (
	// ReconcilerName is the name of the reconciler
	ReconcilerName = "Services"
)

// Reconciler implements controller.Reconciler for Service resources.
type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	serviceLister       listers.ServiceLister
	configurationLister listers.ConfigurationLister
	revisionLister      listers.RevisionLister
	routeLister         listers.RouteLister
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Service resource
// with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	logger := logging.FromContext(ctx)

	// Get the Service resource with this namespace/name
	original, err := c.serviceLister.Services(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logger.Errorf("service %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}

	if original.GetDeletionTimestamp() != nil {
		return nil
	}

	// Don't modify the informers copy
	service := original.DeepCopy()

	// Reconcile this copy of the service and then write back any status
	// updates regardless of whether the reconciliation errored out.
	reconcileErr := c.reconcile(ctx, service)
	if equality.Semantic.DeepEqual(original.Status, service.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.

	} else if _, uErr := c.updateStatus(service); uErr != nil {
		logger.Warnw("Failed to update service status", zap.Error(uErr))
		c.Recorder.Eventf(service, corev1.EventTypeWarning, "UpdateFailed",
			"Failed to update status for Service %q: %v", service.Name, uErr)
		return uErr
	} else if reconcileErr == nil {
		// There was a difference and updateStatus did not return an error.
		c.Recorder.Eventf(service, corev1.EventTypeNormal, "Updated", "Updated Service %q", service.GetName())
	}
	if reconcileErr != nil {
		c.Recorder.Event(service, corev1.EventTypeWarning, "InternalError", reconcileErr.Error())
		return reconcileErr
	}
	// TODO(mattmoor): Remove this after 0.7 cuts.
	// If the spec has changed, then assume we need an upgrade and issue a patch to trigger
	// the webhook to upgrade via defaulting.  Status updates do not trigger this due to the
	// use of the /status resource.
	if !equality.Semantic.DeepEqual(original.Spec, service.Spec) {
		services := v1alpha1.SchemeGroupVersion.WithResource("services")
		if err := c.MarkNeedsUpgrade(services, service.Namespace, service.Name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Reconciler) reconcile(ctx context.Context, service *v1alpha1.Service) error {
	logger := logging.FromContext(ctx)

	// We may be reading a version of the object that was stored at an older version
	// and may not have had all of the assumed defaults specified.  This won't result
	// in this getting written back to the API Server, but lets downstream logic make
	// assumptions about defaulting.
	service.SetDefaults(v1beta1.WithUpgradeViaDefaulting(ctx))
	service.Status.InitializeConditions()

	if err := service.ConvertUp(ctx, &v1beta1.Service{}); err != nil {
		if ce, ok := err.(*v1alpha1.CannotConvertError); ok {
			service.Status.MarkResourceNotConvertible(ce)
			return nil
		}
		return err
	}

	config, err := c.config(ctx, logger, service)
	if err != nil {
		return err
	}

	if config.Generation != config.Status.ObservedGeneration {
		// The Configuration hasn't yet reconciled our latest changes to
		// its desired state, so its conditions are outdated.
		service.Status.MarkConfigurationNotReconciled()
	} else {
		// Update our Status based on the state of our underlying Configuration.
		service.Status.PropagateConfigurationStatus(&config.Status)
	}

	// When the Configuration names a Revision, check that the named Revision is owned
	// by our Configuration and matches its generation before reprogramming the Route,
	// otherwise a bad patch could lead to folks inadvertently routing traffic to a
	// pre-existing Revision (possibly for another Configuration).
	if _, err := cfgreconciler.CheckNameAvailability(config, c.revisionLister); err != nil &&
		!errors.IsNotFound(err) {
		service.Status.MarkRevisionNameTaken(config.Spec.GetTemplate().Name)
		return nil
	}

	route, err := c.route(ctx, logger, service)
	if err != nil {
		return err
	}

	fmt.Printf("\n\n\n\n=========reconciler, route.Generation is %v, route.Status.ObservedGeneration is %v\n\n\n", route.Generation, route.Status.ObservedGeneration)
	// Update our Status based on the state of our underlying Route.
	ss := &service.Status
	if route.Generation != route.Status.ObservedGeneration {
		// The Route hasn't yet reconciled our latest changes to
		// its desired state, so its conditions are outdated.
		ss.MarkRouteNotReconciled()
	} else {
		// Update our Status based on the state of our underlying Route.
		ss.PropagateRouteStatus(&route.Status)
	}

	c.checkRoutesNotReady(config, logger, route, service)
	service.Status.ObservedGeneration = service.Generation

	return nil
}

func (c *Reconciler) config(ctx context.Context, logger *zap.SugaredLogger, service *v1alpha1.Service) (*v1alpha1.Configuration, error) {
	configName := resourcenames.Configuration(service)
	config, err := c.configurationLister.Configurations(service.Namespace).Get(configName)
	if errors.IsNotFound(err) {
		config, err = c.createConfiguration(service)
		if err != nil {
			logger.Errorf("Failed to create Configuration %q: %v", configName, err)
			c.Recorder.Eventf(service, corev1.EventTypeWarning, "CreationFailed", "Failed to create Configuration %q: %v", configName, err)
			return nil, err
		}
		c.Recorder.Eventf(service, corev1.EventTypeNormal, "Created", "Created Configuration %q", configName)
	} else if err != nil {
		logger.Errorw(
			fmt.Sprintf("Failed to reconcile Service: %q failed to Get Configuration: %q", service.Name, configName),
			zap.Error(err))
		return nil, err
	} else if !metav1.IsControlledBy(config, service) {
		// Surface an error in the service's status,and return an error.
		service.Status.MarkConfigurationNotOwned(configName)
		return nil, fmt.Errorf("service: %q does not own configuration: %q", service.Name, configName)
	} else if config, err = c.reconcileConfiguration(ctx, service, config); err != nil {
		logger.Errorw(
			fmt.Sprintf("Failed to reconcile Service: %q failed to reconcile Configuration: %q",
				service.Name, configName), zap.Error(err))
		return nil, err
	}
	return config, nil
}

func (c *Reconciler) route(ctx context.Context, logger *zap.SugaredLogger, service *v1alpha1.Service) (*v1alpha1.Route, error) {
	routeName := resourcenames.Route(service)
	route, err := c.routeLister.Routes(service.Namespace).Get(routeName)
	if errors.IsNotFound(err) {
		route, err = c.createRoute(service)
		if err != nil {
			logger.Errorf("Failed to create Route %q: %v", routeName, err)
			c.Recorder.Eventf(service, corev1.EventTypeWarning, "CreationFailed", "Failed to create Route %q: %v", routeName, err)
			return nil, err
		}
		c.Recorder.Eventf(service, corev1.EventTypeNormal, "Created", "Created Route %q", routeName)
	} else if err != nil {
		logger.Errorf("Failed to reconcile Service: %q failed to Get Route: %q", service.Name, routeName)
		return nil, err
	} else if !metav1.IsControlledBy(route, service) {
		// Surface an error in the service's status, and return an error.
		service.Status.MarkRouteNotOwned(routeName)
		return nil, fmt.Errorf("service: %q does not own route: %q", service.Name, routeName)
	} else if route, err = c.reconcileRoute(ctx, service, route); err != nil {
		logger.Errorf("Failed to reconcile Service: %q failed to reconcile Route: %q", service.Name, routeName)
		return nil, err
	}
	return route, nil
}

func (c *Reconciler) checkRoutesNotReady(config *v1alpha1.Configuration, logger *zap.SugaredLogger, route *v1alpha1.Route, service *v1alpha1.Service) {
	// `manual` is not reconciled.
	rc := service.Status.GetCondition(v1alpha1.ServiceConditionRoutesReady)
	if rc == nil || rc.Status != corev1.ConditionTrue {
		return
	}

	if len(route.Spec.Traffic) != len(route.Status.Traffic) {
		service.Status.MarkRouteNotYetReady()
		return
	}

	want, got := route.Spec.DeepCopy().Traffic, route.Status.DeepCopy().Traffic
	// Replace `configuration` target with its latest ready revision.
	for idx := range want {
		if want[idx].ConfigurationName == config.Name {
			want[idx].RevisionName = config.Status.LatestReadyRevisionName
			want[idx].ConfigurationName = ""
		}
	}
	ignoreFields := cmpopts.IgnoreFields(v1alpha1.TrafficTarget{},
		"TrafficTarget.URL", "TrafficTarget.LatestRevision",
		// We specify the Routing via Tag in spec, but the status surfaces it
		// via both names for now, so ignore the deprecated name field when
		// comparing them.
		"DeprecatedName")
	if diff, err := kmp.SafeDiff(got, want, ignoreFields); err != nil || diff != "" {
		logger.Errorf("Route %q is not yet what we want: %s", service.Name, diff)
		service.Status.MarkRouteNotYetReady()
	}
}

func (c *Reconciler) updateStatus(desired *v1alpha1.Service) (*v1alpha1.Service, error) {
	service, err := c.serviceLister.Services(desired.Namespace).Get(desired.Name)
	if err != nil {
		return nil, err
	}
	// If there's nothing to update, just return.
	if reflect.DeepEqual(service.Status, desired.Status) {
		return service, nil
	}
	becomesReady := desired.Status.IsReady() && !service.Status.IsReady()
	// Don't modify the informers copy.
	existing := service.DeepCopy()
	existing.Status = desired.Status

	svc, err := c.ServingClientSet.ServingV1alpha1().Services(desired.Namespace).UpdateStatus(existing)
	if err == nil && becomesReady {
		duration := time.Since(svc.ObjectMeta.CreationTimestamp.Time)
		c.Logger.Infof("Service %q became ready after %v", service.Name, duration)
		c.StatsReporter.ReportServiceReady(service.Namespace, service.Name, duration)
	}

	return svc, err
}

func (c *Reconciler) createConfiguration(service *v1alpha1.Service) (*v1alpha1.Configuration, error) {
	cfg, err := resources.MakeConfiguration(service)
	if err != nil {
		return nil, err
	}
	return c.ServingClientSet.ServingV1alpha1().Configurations(service.Namespace).Create(cfg)
}

func configSemanticEquals(desiredConfig, config *v1alpha1.Configuration) bool {
	return equality.Semantic.DeepEqual(desiredConfig.Spec, config.Spec) &&
		equality.Semantic.DeepEqual(desiredConfig.ObjectMeta.Labels, config.ObjectMeta.Labels) &&
		equality.Semantic.DeepEqual(desiredConfig.ObjectMeta.Annotations, config.ObjectMeta.Annotations)
}

func (c *Reconciler) reconcileConfiguration(ctx context.Context, service *v1alpha1.Service, config *v1alpha1.Configuration) (*v1alpha1.Configuration, error) {
	logger := logging.FromContext(ctx)
	desiredConfig, err := resources.MakeConfiguration(service)
	if err != nil {
		return nil, err
	}

	if configSemanticEquals(desiredConfig, config) {
		// No differences to reconcile.
		return config, nil
	}
	diff, err := kmp.SafeDiff(desiredConfig.Spec, config.Spec)
	if err != nil {
		return nil, fmt.Errorf("failed to diff Configuration: %v", err)
	}
	logger.Infof("Reconciling configuration diff (-desired, +observed): %s", diff)

	// Don't modify the informers copy.
	existing := config.DeepCopy()
	// Preserve the rest of the object (e.g. ObjectMeta except for labels).
	existing.Spec = desiredConfig.Spec
	existing.ObjectMeta.Labels = desiredConfig.ObjectMeta.Labels
	existing.ObjectMeta.Annotations = desiredConfig.ObjectMeta.Annotations
	return c.ServingClientSet.ServingV1alpha1().Configurations(service.Namespace).Update(existing)
}

func (c *Reconciler) createRoute(service *v1alpha1.Service) (*v1alpha1.Route, error) {
	route, err := resources.MakeRoute(service)
	if err != nil {
		// This should be unreachable as configuration creation
		// happens first in `reconcile()` and it verifies the edge cases
		// that would make `MakeRoute` fail as well.
		return nil, err
	}
	return c.ServingClientSet.ServingV1alpha1().Routes(service.Namespace).Create(route)
}

func routeSemanticEquals(desiredRoute, route *v1alpha1.Route) bool {
	return equality.Semantic.DeepEqual(desiredRoute.Spec, route.Spec) &&
		equality.Semantic.DeepEqual(desiredRoute.ObjectMeta.Labels, route.ObjectMeta.Labels) &&
		equality.Semantic.DeepEqual(desiredRoute.ObjectMeta.Annotations, route.ObjectMeta.Annotations)
}

func (c *Reconciler) reconcileRoute(ctx context.Context, service *v1alpha1.Service, route *v1alpha1.Route) (*v1alpha1.Route, error) {
	logger := logging.FromContext(ctx)
	fmt.Printf("\n\n\n\n\n --------reconcileRoute \n\n\n")
	desiredRoute, err := resources.MakeRoute(service)
	if err != nil {
		// This should be unreachable as configuration creation
		// happens first in `reconcile()` and it verifies the edge cases
		// that would make `MakeRoute` fail as well.
		return nil, err
	}

	if routeSemanticEquals(desiredRoute, route) {
		// No differences to reconcile.
		fmt.Printf("\n\n\n\n\n No differences to reconcile. desiredRoute is \n%v\n\n, route is \n%v\n\n\n\n\n\n", desiredRoute, route)
		return route, nil
	}
	diff, err := kmp.SafeDiff(desiredRoute.Spec, route.Spec)
	if err != nil {
		fmt.Printf("\n\n\n\n\n failed to diff Route \n\n\n")
		return nil, fmt.Errorf("failed to diff Route: %v", err)
	}
	logger.Infof("Reconciling route diff (-desired, +observed): %s", diff)
	fmt.Printf("\n\n\n\n\n Reconciling route diff (-desired, +observed): %s\n\n\n", diff)

	// Don't modify the informers copy.
	existing := route.DeepCopy()
	// Preserve the rest of the object (e.g. ObjectMeta except for labels and annotations).
	existing.Spec = desiredRoute.Spec
	existing.ObjectMeta.Labels = desiredRoute.ObjectMeta.Labels
	existing.ObjectMeta.Annotations = desiredRoute.ObjectMeta.Annotations

	rc := route.Status.GetCondition(apis.ConditionReady)
	if rc != nil && rc.Status == corev1.ConditionFalse {
		fmt.Printf("\n\n\n\n\n route existing.Status.ObservedGeneration++!!!!!!!! \n\n\n\n\n\n")
		existing.Status.ObservedGeneration++
	}

	newRoute, err := c.ServingClientSet.ServingV1alpha1().Routes(service.Namespace).Update(existing)
	fmt.Printf("\n\n\n\n=============++++++++ route is now updated to %v \n\n", newRoute)
	if newRoute != nil {
		fmt.Printf("\n\n\n\n=============++++++++ newRoute.Generation is %v, newRoute.Status.ObservedGeneration is %v, newRoute.Spec is %v\n\n\n", newRoute.Generation, newRoute.Status.ObservedGeneration, newRoute.Spec)
	}
	return newRoute, err
}
