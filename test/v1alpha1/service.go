/*
Copyright 2019 The Knative Authors

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

// service.go provides methods to perform actions on the service resource.

package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"

	"knative.dev/pkg/apis/duck"
	"knative.dev/pkg/reconciler"

	"github.com/mattbaird/jsonpatch"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/test/logging"
	"knative.dev/serving/pkg/apis/serving/v1alpha1"
	serviceresourcenames "knative.dev/serving/pkg/reconciler/service/resources/names"

	pkgTest "knative.dev/pkg/test"
	rtesting "knative.dev/serving/pkg/testing/v1alpha1"
	"knative.dev/serving/test"
)

func validateCreatedServiceStatus(clients *test.Clients, names *test.ResourceNames) error {
	return CheckServiceState(clients.ServingAlphaClient, names.Service, func(s *v1alpha1.Service) (bool, error) {
		if s.Status.URL == nil || s.Status.URL.Host == "" {
			return false, fmt.Errorf("URL is not present in Service status: %v", s)
		}
		names.URL = s.Status.URL.URL()
		if s.Status.LatestCreatedRevisionName == "" {
			return false, fmt.Errorf("LatestCreatedCreatedRevisionName is not present in Service status: %v", s)
		}
		names.Revision = s.Status.LatestCreatedRevisionName
		if s.Status.LatestReadyRevisionName == "" {
			return false, fmt.Errorf("LatestReadyRevisionName is not present in Service status: %v", s)
		}
		if s.Status.ObservedGeneration != 1 {
			return false, fmt.Errorf("ObservedGeneration is not 1 in Service status: %v", s)
		}
		return true, nil
	})
}

// GetResourceObjects obtains the services resources from the k8s API server.
func GetResourceObjects(clients *test.Clients, names test.ResourceNames) (*ResourceObjects, error) {
	routeObject, err := clients.ServingAlphaClient.Routes.Get(context.Background(), names.Route, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	serviceObject, err := clients.ServingAlphaClient.Services.Get(context.Background(), names.Service, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	configObject, err := clients.ServingAlphaClient.Configs.Get(context.Background(), names.Config, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	revisionObject, err := clients.ServingAlphaClient.Revisions.Get(context.Background(), names.Revision, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return &ResourceObjects{
		Route:    routeObject,
		Service:  serviceObject,
		Config:   configObject,
		Revision: revisionObject,
	}, nil
}

// CreateRunLatestServiceReady creates a new Service in state 'Ready'. This function expects Service and Image name passed in through 'names'.
// Names is updated with the Route and Configuration created by the Service and ResourceObjects is returned with the Service, Route, and Configuration objects.
// Returns error if the service does not come up correctly.
func CreateRunLatestServiceReady(t pkgTest.TLegacy, clients *test.Clients, names *test.ResourceNames, fopt ...rtesting.ServiceOption) (*ResourceObjects, error) {
	if names.Image == "" {
		return nil, fmt.Errorf("expected non-empty Image name; got Image=%v", names.Image)
	}

	t.Log("Creating a new Service.", "service", names.Service)
	svc, err := CreateLatestService(t, clients, *names, fopt...)
	if err != nil {
		return nil, err
	}

	// Populate Route and Configuration Objects with name
	names.Route = serviceresourcenames.Route(svc)
	names.Config = serviceresourcenames.Configuration(svc)

	// If the Service name was not specified, populate it
	if names.Service == "" {
		names.Service = svc.Name
	}

	t.Log("Waiting for Service to transition to Ready.", "service", names.Service)
	if err = WaitForServiceState(clients.ServingAlphaClient, names.Service, IsServiceReady, "ServiceIsReady"); err != nil {
		return nil, err
	}

	t.Log("Checking to ensure Service Status is populated for Ready service")
	err = validateCreatedServiceStatus(clients, names)
	if err != nil {
		return nil, err
	}

	t.Log("Getting latest objects Created by Service")
	resources, err := GetResourceObjects(clients, *names)
	if err == nil {
		t.Log("Successfully created Service", names.Service)
	}
	return resources, err
}

// CreateRunLatestServiceLegacyReady creates a new Service in state 'Ready'. This function expects Service and Image name passed in through 'names'.
// Names is updated with the Route and Configuration created by the Service and ResourceObjects is returned with the Service, Route, and Configuration objects.
// Returns error if the service does not come up correctly.
func CreateRunLatestServiceLegacyReady(t pkgTest.T, clients *test.Clients, names *test.ResourceNames, fopt ...rtesting.ServiceOption) (*ResourceObjects, error) {
	if names.Image == "" {
		return nil, fmt.Errorf("expected non-empty Image name; got Image=%v", names.Image)
	}

	t.Log("Creating a new Service.", "service", names.Service)
	svc, err := CreateLatestServiceLegacy(t, clients, *names, fopt...)
	if err != nil {
		return nil, err
	}

	// Populate Route and Configuration Objects with name
	names.Route = serviceresourcenames.Route(svc)
	names.Config = serviceresourcenames.Configuration(svc)

	// If the Service name was not specified, populate it
	if names.Service == "" {
		names.Service = svc.Name
	}

	t.Log("Waiting for Service to transition to Ready.", "service", names.Service)
	if err := WaitForServiceState(clients.ServingAlphaClient, names.Service, IsServiceReady, "ServiceIsReady"); err != nil {
		return nil, err
	}

	t.Log("Checking to ensure Service Status is populated for Ready service", names.Service)
	err = validateCreatedServiceStatus(clients, names)
	if err != nil {
		return nil, err
	}

	t.Log("Getting latest objects Created by Service", names.Service)
	resources, err := GetResourceObjects(clients, *names)
	if err == nil {
		t.Log("Successfully created Service", names.Service)
	}
	return resources, err
}

// CreateLatestService creates a service in namespace with the name names.Service and names.Image
func CreateLatestService(t pkgTest.T, clients *test.Clients, names test.ResourceNames, fopt ...rtesting.ServiceOption) (svc *v1alpha1.Service, err error) {
	service := LatestService(names, fopt...)
	LogResourceObject(t, ResourceObjects{Service: service})
	return svc, reconciler.RetryTestErrors(func(int) (err error) {
		svc, err = clients.ServingAlphaClient.Services.Create(context.Background(), service, metav1.CreateOptions{})
		return err
	})
}

// CreateLatestServiceLegacy creates a service in namespace with the name names.Service and names.Image
func CreateLatestServiceLegacy(t pkgTest.T, clients *test.Clients, names test.ResourceNames, fopt ...rtesting.ServiceOption) (*v1alpha1.Service, error) {
	service := LatestServiceLegacy(names, fopt...)
	test.AddTestAnnotation(t, service.ObjectMeta)
	LogResourceObject(t, ResourceObjects{Service: service})
	svc, err := clients.ServingAlphaClient.Services.Create(context.Background(), service, metav1.CreateOptions{})
	return svc, err
}

// PatchServiceImage patches the existing service passed in with a new imagePath. Returns the latest service object
func PatchServiceImage(t pkgTest.T, clients *test.Clients, service *v1alpha1.Service, imagePath string) (svc *v1alpha1.Service, err error) {
	newSvc := service.DeepCopy()
	if service.Spec.DeprecatedRunLatest != nil {
		newSvc.Spec.DeprecatedRunLatest.Configuration.GetTemplate().Spec.GetContainer().Image = imagePath
	} else if service.Spec.DeprecatedRelease != nil {
		newSvc.Spec.DeprecatedRelease.Configuration.GetTemplate().Spec.GetContainer().Image = imagePath
	} else if service.Spec.DeprecatedPinned != nil {
		newSvc.Spec.DeprecatedPinned.Configuration.GetTemplate().Spec.GetContainer().Image = imagePath
	} else {
		newSvc.Spec.ConfigurationSpec.GetTemplate().Spec.GetContainer().Image = imagePath
	}
	LogResourceObject(t, ResourceObjects{Service: newSvc})
	patchBytes, err := duck.CreateBytePatch(service, newSvc)
	if err != nil {
		return nil, err
	}
	return svc, reconciler.RetryTestErrors(func(int) (err error) {
		svc, err = clients.ServingAlphaClient.Services.Patch(context.Background(), service.ObjectMeta.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		return err
	})
}

// PatchService creates and applies a patch from the diff between curSvc and desiredSvc. Returns the latest service object.
func PatchService(t pkgTest.T, clients *test.Clients, curSvc, desiredSvc *v1alpha1.Service) (svc *v1alpha1.Service, err error) {
	LogResourceObject(t, ResourceObjects{Service: desiredSvc})
	patchBytes, err := duck.CreateBytePatch(curSvc, desiredSvc)
	if err != nil {
		return nil, err
	}
	return svc, reconciler.RetryTestErrors(func(int) (err error) {
		svc, err = clients.ServingAlphaClient.Services.Patch(context.Background(), curSvc.ObjectMeta.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		return err
	})
}

// UpdateServiceRouteSpec updates a service to use the route name in names.
func UpdateServiceRouteSpec(t pkgTest.T, clients *test.Clients, names test.ResourceNames, rs v1alpha1.RouteSpec) (svc *v1alpha1.Service, err error) {
	patches := []jsonpatch.JsonPatchOperation{{
		Operation: "replace",
		Path:      "/spec/traffic",
		Value:     rs.Traffic,
	}}
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return nil, err
	}
	return svc, reconciler.RetryTestErrors(func(int) (err error) {
		svc, err = clients.ServingAlphaClient.Services.Patch(context.Background(), names.Service, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		return err
	})
}

// PatchServiceTemplateMetadata patches an existing service by adding metadata to the service's RevisionTemplateSpec.
func PatchServiceTemplateMetadata(t pkgTest.T, clients *test.Clients, service *v1alpha1.Service, metadata metav1.ObjectMeta) (svc *v1alpha1.Service, err error) {
	newSvc := service.DeepCopy()
	newSvc.Spec.ConfigurationSpec.Template.ObjectMeta = metadata
	LogResourceObject(t, ResourceObjects{Service: newSvc})
	patchBytes, err := duck.CreateBytePatch(service, newSvc)
	if err != nil {
		return nil, err
	}
	return svc, reconciler.RetryTestErrors(func(int) (err error) {
		svc, err = clients.ServingAlphaClient.Services.Patch(context.Background(), service.ObjectMeta.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		return err
	})
}

// WaitForServiceLatestRevision takes a revision in through names and compares it to the current state of LatestCreatedRevisionName in Service.
// Once an update is detected in the LatestCreatedRevisionName, the function waits for the created revision to be set in LatestReadyRevisionName
// before returning the name of the revision.
func WaitForServiceLatestRevision(clients *test.Clients, names test.ResourceNames) (string, error) {
	var revisionName string
	if err := WaitForServiceState(clients.ServingAlphaClient, names.Service, func(s *v1alpha1.Service) (bool, error) {
		if s.Status.LatestCreatedRevisionName != names.Revision {
			revisionName = s.Status.LatestCreatedRevisionName
			// We also check that the revision is pinned, meaning it's not a stale revision.
			// Without this it might happen that the latest created revision is later overridden by a newer one
			// and the following check for LatestReadyRevisionName would fail.
			if revErr := CheckRevisionState(clients.ServingAlphaClient, revisionName, IsRevisionPinned); revErr != nil {
				return false, nil
			}
			return true, nil
		}
		return false, nil
	}, "ServiceUpdatedWithRevision"); err != nil {
		return "", fmt.Errorf("LatestCreatedRevisionName not updated: %w", err)
	}
	if err := WaitForServiceState(clients.ServingAlphaClient, names.Service, func(s *v1alpha1.Service) (bool, error) {
		return (s.Status.LatestReadyRevisionName == revisionName), nil
	}, "ServiceReadyWithRevision"); err != nil {
		return "", fmt.Errorf("LatestReadyRevisionName not updated with %s: %w", revisionName, err)
	}

	return revisionName, nil
}

// LatestService returns a Service object in namespace with the name names.Service
// that uses the image specified by names.Image.
func LatestService(names test.ResourceNames, fopt ...rtesting.ServiceOption) *v1alpha1.Service {
	a := append([]rtesting.ServiceOption{
		rtesting.WithInlineConfigSpec(*ConfigurationSpec(pkgTest.ImagePath(names.Image))),
	}, fopt...)
	return rtesting.ServiceWithoutNamespace(names.Service, a...)
}

// LatestServiceLegacy returns a DeprecatedRunLatest Service object in namespace with the name names.Service
// that uses the image specified by names.Image.
func LatestServiceLegacy(names test.ResourceNames, fopt ...rtesting.ServiceOption) *v1alpha1.Service {
	a := append([]rtesting.ServiceOption{
		rtesting.WithRunLatestConfigSpec(*LegacyConfigurationSpec(pkgTest.ImagePath(names.Image))),
	}, fopt...)
	svc := rtesting.ServiceWithoutNamespace(names.Service, a...)
	// Clear the name, which is put there by defaulting.
	svc.Spec.DeprecatedRunLatest.Configuration.GetTemplate().Spec.GetContainer().Name = ""
	return svc
}

// WaitForServiceState polls the status of the Service called name
// from client every `PollInterval` until `inState` returns `true` indicating it
// is done, returns an error or PollTimeout. desc will be used to name the metric
// that is emitted to track how long it took for name to get into the state checked by inState.
func WaitForServiceState(client *test.ServingAlphaClients, name string, inState func(s *v1alpha1.Service) (bool, error), desc string) error {
	span := logging.GetEmitableSpan(context.Background(), fmt.Sprintf("WaitForServiceState/%s/%s", name, desc))
	defer span.End()

	var lastState *v1alpha1.Service
	waitErr := wait.PollImmediate(test.PollInterval, test.PollTimeout, func() (bool, error) {
		err := reconciler.RetryTestErrors(func(int) (err error) {
			lastState, err = client.Services.Get(context.Background(), name, metav1.GetOptions{})
			return err
		})
		if err != nil {
			return true, err
		}
		return inState(lastState)
	})

	if waitErr != nil {
		return fmt.Errorf("service %q is not in desired state, got: %#v: %w", name, lastState, waitErr)
	}
	return nil
}

// CheckServiceState verifies the status of the Service called name from client
// is in a particular state by calling `inState` and expecting `true`.
// This is the non-polling variety of WaitForServiceState.
func CheckServiceState(client *test.ServingAlphaClients, name string, inState func(s *v1alpha1.Service) (bool, error)) error {
	var s *v1alpha1.Service
	err := reconciler.RetryTestErrors(func(int) (err error) {
		s, err = client.Services.Get(context.Background(), name, metav1.GetOptions{})
		return err
	})
	if err != nil {
		return err
	}
	if done, err := inState(s); err != nil {
		return err
	} else if !done {
		return fmt.Errorf("service %q is not in desired state, got: %#v", name, s)
	}
	return nil
}

// IsServiceReady will check the status conditions of the service and return true if the service is
// ready. This means that its configurations and routes have all reported ready.
func IsServiceReady(s *v1alpha1.Service) (bool, error) {
	return s.Generation == s.Status.ObservedGeneration && s.Status.IsReady(), nil
}

// IsServiceNotReady checks the Ready status condition of the service and returns true only if Ready is set to False.
func IsServiceNotReady(s *v1alpha1.Service) (bool, error) {
	result := s.Status.GetCondition(v1alpha1.ServiceConditionReady)
	return s.Generation == s.Status.ObservedGeneration && result != nil && result.Status == corev1.ConditionFalse, nil
}

// IsServiceAndChildrenFailed will check the readiness, route and config conditions of the service
// and return true if they are all failed.
func IsServiceAndChildrenFailed(s *v1alpha1.Service) (bool, error) {
	if s.Generation != s.Status.ObservedGeneration {
		return false, nil
	}

	readyCond := s.Status.GetCondition(v1alpha1.ServiceConditionReady)
	if readyCond == nil || readyCond.Status != corev1.ConditionFalse {
		return false, nil
	}

	routeCond := s.Status.GetCondition(v1alpha1.ServiceConditionRoutesReady)
	if routeCond == nil || routeCond.Status != corev1.ConditionFalse {
		return false, nil
	}

	configCond := s.Status.GetCondition(v1alpha1.ServiceConditionConfigurationsReady)
	if configCond == nil || configCond.Status != corev1.ConditionFalse {
		return false, nil
	}

	return true, nil
}

// IsServiceRoutesNotReady checks the RoutesReady status of the service and returns true only if RoutesReady is set to False.
func IsServiceRoutesNotReady(s *v1alpha1.Service) (bool, error) {
	result := s.Status.GetCondition(v1alpha1.ServiceConditionRoutesReady)
	return s.Generation == s.Status.ObservedGeneration && result != nil && result.Status == corev1.ConditionFalse, nil
}
