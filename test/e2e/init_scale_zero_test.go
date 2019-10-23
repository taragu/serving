// +build e2e

/*
Copyright 2020 The Knative Authors

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

package e2e

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/test/logstream"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/apis/serving"
	v1a1testing "knative.dev/serving/pkg/testing/v1alpha1"
	"knative.dev/serving/test"
	v1a1test "knative.dev/serving/test/v1alpha1"
)

// TestInitScaleZeroServiceLevel tests setting of annotation checkValidityOnDeploy on
// the revision level
func TestInitScaleZeroServiceLevel(t *testing.T) {
	t.Parallel()
	cancel := logstream.Start(t)
	defer cancel()

	clients := Setup(t)
	names := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}

	test.CleanupOnInterrupt(func() { test.TearDown(clients, names) })
	defer test.TearDown(clients, names)

	t.Log("Creating a new Service with check validity on deploy being false.")
	objects, _, err := v1a1test.CreateRunLatestServiceReady(t, clients, &names,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.CheckValidityOnDeployAnnotation: "false",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", names.Service, err)
	}

	// Verify that no pods are created
	pods := clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err := pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", names.Service, err)
	}
	if len(podList.Items) != 0 {
		t.Fatalf("Did not scale to 0; %d pods are created.", len(podList.Items))
	}

	t.Log("Creating a new Service with check validity on deploy being true.")
	names = test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}
	objects, _, err = v1a1test.CreateRunLatestServiceReady(t, clients, &names,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.CheckValidityOnDeployAnnotation: "true",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", names.Service, err)
	}

	// Verify that pods are created
	pods = clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err = pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", names.Service, err)
	}
	if len(podList.Items) == 0 {
		t.Fatal("No pods are created.")
	}
}

// TestInitScaleZeroClusterLevel tests setting of annotation checkValidityOnDeploy on
// the cluster level
func TestInitScaleZeroClusterLevel(t *testing.T) {
	cancel := logstream.Start(t)
	defer cancel()

	clients := Setup(t)
	namesCheckValidityUnset := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}
	namesCheckValidityFalse := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}
	namesCheckValidityTrue := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}

	test.CleanupOnInterrupt(func() {
		test.TearDown(clients, namesCheckValidityUnset)
		test.TearDown(clients, namesCheckValidityFalse)
		test.TearDown(clients, namesCheckValidityTrue)

	})
	defer test.TearDown(clients, namesCheckValidityUnset)
	defer test.TearDown(clients, namesCheckValidityFalse)
	defer test.TearDown(clients, namesCheckValidityTrue)

	t.Log("Fetch config-defaults ConfigMap")
	configDefaults, err := defaultCM(clients)
	if err != nil || configDefaults == nil {
		t.Fatalf("Error fetching config-defaults: %v", err)
	} else {
		var newCM *v1.ConfigMap
		configDefaults.Data["check-validity-on-deploy"] = "false"
		if newCM, err = patchDefaultCM(clients, configDefaults); err != nil {
			t.Fatalf("Failed to update config defaults: %v", err)
		}
		t.Logf("Successfully updated configMap.")
		newCM.Data["check-validity-on-deploy"] = "true"
		test.CleanupOnInterrupt(func() { restoreClusterDefaults(t, clients, newCM) })
		defer restoreClusterDefaults(t, clients, newCM)
	}

	t.Log("Creating a new Service without check validity on deploy being set.")
	objects, _, err := v1a1test.CreateRunLatestServiceReady(t, clients, &namesCheckValidityUnset,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */)
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", namesCheckValidityUnset.Service, err)
	}
	// Verify that no pods are created
	pods := clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err := pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", namesCheckValidityUnset.Service, err)
	}
	if len(podList.Items) != 0 {
		t.Fatalf("Did not scale to 0; %d pods are created for revision %s.", len(podList.Items), objects.Revision.Name)
	}

	t.Log("Creating a new Service with check validity on deploy being false.")
	objects, _, err = v1a1test.CreateRunLatestServiceReady(t, clients, &namesCheckValidityFalse,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.CheckValidityOnDeployAnnotation: "false",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", namesCheckValidityFalse.Service, err)
	}

	// Verify that no pods are created
	pods = clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err = pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", namesCheckValidityFalse.Service, err)
	}
	if len(podList.Items) != 0 {
		t.Fatalf("Did not scale to 0; %d pods are created.", len(podList.Items))
	}

	t.Log("Creating a new Service with check validity on deploy being true.")
	objects, _, err = v1a1test.CreateRunLatestServiceReady(t, clients, &namesCheckValidityTrue,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.CheckValidityOnDeployAnnotation: "true",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", namesCheckValidityTrue.Service, err)
	}

	// Verify that pods are created
	pods = clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err = pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", namesCheckValidityTrue.Service, err)
	}
	if len(podList.Items) == 0 {
		t.Fatal("No pods are created.")
	}
}

// TestInitScaleZeroMinScaleClusterLevel tests setting of annotation checkValidityOnDeploy on
// the cluster level with minScale > 0
func TestInitScaleZeroMinScaleClusterLevel(t *testing.T) {
	cancel := logstream.Start(t)
	defer cancel()

	clients := Setup(t)
	names := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}

	test.CleanupOnInterrupt(func() {
		test.TearDown(clients, names)

	})
	defer test.TearDown(clients, names)

	t.Log("Fetch config-defaults ConfigMap")
	configDefaults, err := defaultCM(clients)
	if err != nil || configDefaults == nil {
		t.Fatalf("Error fetching config-defaults: %v", err)
	} else {
		var newCM *v1.ConfigMap
		configDefaults.Data["check-validity-on-deploy"] = "false"
		if newCM, err = patchDefaultCM(clients, configDefaults); err != nil {
			t.Fatalf("Failed to update config defaults: %v", err)
		}
		t.Logf("Successfully updated configMap.")
		newCM.Data["check-validity-on-deploy"] = "true"
		test.CleanupOnInterrupt(func() { restoreClusterDefaults(t, clients, newCM) })
		defer restoreClusterDefaults(t, clients, newCM)
	}

	t.Log("Creating a new Service with minScale greater than 0.")
	objects, _, err := v1a1test.CreateRunLatestServiceReady(t, clients, &names,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.MinScaleAnnotationKey: "1",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", names.Service, err)
	}

	// Verify that pods are created
	pods := clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err := pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", names.Service, err)
	}
	if len(podList.Items) == 0 {
		t.Fatal("No pods are created.")
	}
}

// TestInitScaleZeroMinScaleServiceLevel tests setting of annotation checkValidityOnDeploy on
// the service level with minScale > 0
func TestInitScaleZeroMinScaleServiceLevel(t *testing.T) {
	cancel := logstream.Start(t)
	defer cancel()

	clients := Setup(t)
	names := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "helloworld",
	}

	test.CleanupOnInterrupt(func() {
		test.TearDown(clients, names)

	})
	defer test.TearDown(clients, names)

	t.Log("Creating a new Service with check validity on deploy being false and minScale greater than 0.")
	objects, _, err := v1a1test.CreateRunLatestServiceReady(t, clients, &names,
		false, /* https TODO(taragu) turn this on after helloworld test running with https */
		v1a1testing.WithConfigAnnotations(map[string]string{
			autoscaling.CheckValidityOnDeployAnnotation: "false",
			autoscaling.MinScaleAnnotationKey:       "1",
		}))
	if err != nil {
		t.Fatalf("Failed to create initial Service: %v: %v", names.Service, err)
	}

	// Verify that pods are created
	pods := clients.KubeClient.Kube.CoreV1().Pods(test.ServingNamespace)
	podList, err := pods.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", serving.RevisionLabelKey, objects.Revision.Name),
	})
	if err != nil {
		t.Fatalf("Failed to list all pods: %v: %v", names.Service, err)
	}
	if len(podList.Items) == 0 {
		t.Fatal("No pods are created.")
	}
}

func restoreClusterDefaults(t *testing.T, clients *test.Clients, oldCM *v1.ConfigMap) {
	if _, err := patchDefaultCM(clients, oldCM); err != nil {
		t.Fatalf("Error restoring config-defaults: %v", err)
	}
	t.Log("Successfully restored config-defaults.")
}
